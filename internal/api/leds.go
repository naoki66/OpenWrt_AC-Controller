package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/internal/discovery"
	"github.com/naoki66/ac-controller/internal/proto"
	"github.com/naoki66/ac-controller/internal/store"
)

// handleSetLEDSchedule stores a per-AP daily status-LED policy. The controller
// only declares intent here; the actual switch goes through the same action
// queue as a manual one, so an offline AP picks it up on return.
func (s *Server) handleSetLEDSchedule(w http.ResponseWriter, r *http.Request) {
	mac := discovery.NormalizeMAC(r.PathValue("mac"))
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("malformed MAC address"))
		return
	}
	device, err := s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, errors.New("device not found"))
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if device.State != store.StateAdopted {
		writeError(w, http.StatusConflict, errors.New("device is not adopted"))
		return
	}

	var req struct {
		OnAt  string `json:"on_at"`
		OffAt string `json:"off_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	onAt := strings.TrimSpace(req.OnAt)
	offAt := strings.TrimSpace(req.OffAt)
	if onAt != "" && offAt != "" && onAt == offAt {
		writeError(w, http.StatusBadRequest, errors.New("on_at and off_at must differ"))
		return
	}

	now := s.now()
	onNext, offNext := int64(0), int64(0)
	if onAt != "" {
		when, err := nextClockOccurrence(now, onAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		onNext = when.Unix()
	}
	if offAt != "" {
		when, err := nextClockOccurrence(now, offAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		offNext = when.Unix()
	}

	if err := s.Store.SetLEDSchedule(r.Context(), mac, onAt, offAt, onNext, offNext); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "led_schedule", mac, formatLEDSchedule(onAt, offAt))

	device, err = s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func formatLEDSchedule(onAt, offAt string) string {
	if onAt == "" && offAt == "" {
		return "disabled"
	}
	parts := make([]string, 0, 2)
	if onAt != "" {
		parts = append(parts, "on "+onAt)
	}
	if offAt != "" {
		parts = append(parts, "off "+offAt)
	}
	return strings.Join(parts, ", ")
}

// RunLEDScheduler fires due LED schedules once a minute until ctx is cancelled.
func (s *Server) RunLEDScheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	s.runDueLEDs(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runDueLEDs(ctx)
		}
	}
}

type ledEvent struct {
	at    int64
	on    bool
	state string
	clock string
}

func (s *Server) runDueLEDs(ctx context.Context) {
	now := s.now()
	devices, err := s.Store.DueLEDSchedules(ctx, now.Unix(), int64(s.offlineAfter()/time.Second), 32)
	if err != nil {
		s.Log.Warn("LED schedule query failed", "err", err)
		return
	}
	for _, d := range devices {
		ev, ok := earliestLEDEvent(d, now.Unix())
		if !ok {
			continue
		}
		// Move the due time first. Whether the switch is issued or deliberately
		// skipped, the next occurrence must be in the future or every tick would
		// re-queue the same schedule.
		next, err := nextClockOccurrence(now, ev.clock)
		if err != nil {
			s.Log.Warn("LED schedule has a bad clock", "mac", d.MAC, "at", ev.clock, "err", err)
			continue
		}
		if err := s.Store.AdvanceLEDSchedule(ctx, d.MAC, ev.on, next.Unix()); err != nil {
			s.Log.Warn("LED schedule not advanced", "mac", d.MAC, "err", err)
			continue
		}

		pending, err := s.Store.HasPendingAction(ctx, d.MAC, proto.ActionLEDSet)
		if err != nil {
			s.Log.Warn("pending LED action check failed", "mac", d.MAC, "err", err)
			continue
		}
		if pending {
			// An operator already asked for a switch; stacking another would
			// apply two opposite states back to back.
			s.Log.Info("scheduled LED skipped: one is already pending", "mac", d.MAC)
			continue
		}

		a, err := s.Store.CreateAction(ctx, d.MAC, proto.ActionLEDSet, map[string]string{"state": ev.state})
		if err != nil {
			s.Log.Warn("scheduled LED not queued", "mac", d.MAC, "err", err)
			continue
		}
		s.Store.LogEvent(ctx, "led_scheduled", d.MAC, a.ID+" "+ev.state)
		s.Log.Info("scheduled LED queued", "mac", d.MAC, "id", a.ID, "state", ev.state, "next", next)
	}
}

// earliestLEDEvent picks one due edge. If both were missed, the earlier one
// runs this tick and the other stays due for the next tick; that keeps the
// final state deterministic instead of relying on queue ordering.
func earliestLEDEvent(d store.Device, now int64) (ledEvent, bool) {
	var events []ledEvent
	if d.LEDOnNextAt > 0 && d.LEDOnNextAt <= now {
		events = append(events, ledEvent{at: d.LEDOnNextAt, on: true, state: "on", clock: d.LEDOnAt})
	}
	if d.LEDOffNextAt > 0 && d.LEDOffNextAt <= now {
		events = append(events, ledEvent{at: d.LEDOffNextAt, on: false, state: "off", clock: d.LEDOffAt})
	}
	if len(events) == 0 {
		return ledEvent{}, false
	}
	ev := events[0]
	for _, e := range events[1:] {
		// On a tie the off edge wins: leaving the LED dark is the safer
		// outcome when two scheduled events collide.
		if e.at < ev.at || (e.at == ev.at && !e.on) {
			ev = e
		}
	}
	return ev, true
}
