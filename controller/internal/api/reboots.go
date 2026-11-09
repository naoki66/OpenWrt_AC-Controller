package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/naoki66/ac-controller/controller/internal/discovery"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/proto"
)

// maxRebootPeriodDays bounds the schedule to something an operator can reason
// about. A value of 0 disables the schedule.
const maxRebootPeriodDays = 365

// handleSetRebootSchedule stores a per-AP periodic reboot policy. The
// controller only declares intent here; the actual reboot goes through the
// same action queue as a manual one, so an offline AP picks it up on return.
func (s *Server) handleSetRebootSchedule(w http.ResponseWriter, r *http.Request) {
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
		PeriodDays int    `json:"period_days"` // legacy "every N days" interval
		Period     string `json:"period"`      // "", "daily", "weekly", "monthly"
		Weekday    int    `json:"weekday"`     // 0-6 (time.Weekday), period=weekly
		Monthday   int    `json:"monthday"`    // 1-28, period=monthly
		At         string `json:"at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// Normalize the request into (days, weekday, monthday). An explicit period
	// wins; the bare period_days form stays working for older clients.
	days, weekday, monthday := req.PeriodDays, -1, 0
	switch req.Period {
	case "":
		// legacy interval mode, unchanged
	case "daily":
		days, weekday, monthday = 1, -1, 0
	case "weekly":
		if req.Weekday < 0 || req.Weekday > 6 {
			writeError(w, http.StatusBadRequest, errors.New("weekday must be 0-6"))
			return
		}
		days, weekday, monthday = 7, req.Weekday, 0
	case "monthly":
		if req.Monthday < 1 || req.Monthday > 28 {
			writeError(w, http.StatusBadRequest, errors.New("monthday must be 1-28"))
			return
		}
		days, weekday, monthday = 30, -1, req.Monthday
	default:
		writeError(w, http.StatusBadRequest, errors.New("period must be daily, weekly or monthly"))
		return
	}
	if days < 0 || days > maxRebootPeriodDays {
		writeError(w, http.StatusBadRequest, errors.New("period_days must be 0-365"))
		return
	}

	next := int64(0)
	if days > 0 {
		when, err := nextRebootOccurrence(s.now(), days, weekday, monthday, req.At)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		next = when.Unix()
	} else {
		req.At = ""
	}

	if err := s.Store.SetRebootSchedule(r.Context(), mac, days, weekday, monthday, req.At, next); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.Store.LogEvent(r.Context(), "reboot_schedule", mac,
		formatSchedule(days, weekday, monthday, req.At))

	device, err = s.Store.GetDevice(r.Context(), mac)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, device)
}

// RunRebootScheduler fires due schedules once a minute until ctx is cancelled.
// One minute is finer than any human-readable schedule and coarse enough that
// the query is negligible.
func (s *Server) RunRebootScheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	s.runDueReboots(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runDueReboots(ctx)
		}
	}
}

func (s *Server) runDueReboots(ctx context.Context) {
	now := s.now()
	devices, err := s.Store.DueReboots(ctx, now.Unix(), int64(s.offlineAfter()/time.Second), 32)
	if err != nil {
		s.Log.Warn("reboot schedule query failed", "err", err)
		return
	}
	for _, d := range devices {
		// Move the due time first. Whether the reboot is issued or deliberately
		// skipped, the next occurrence must be in the future or every tick would
		// re-queue the same schedule.
		next := advanceReboot(time.Unix(d.RebootNextAt, 0), d.RebootPeriodDays, d.RebootWeekday, d.RebootMonthday, now)
		if err := s.Store.AdvanceRebootSchedule(ctx, d.MAC, next.Unix()); err != nil {
			s.Log.Warn("reboot schedule not advanced", "mac", d.MAC, "err", err)
			continue
		}

		pending, err := s.Store.HasPendingAction(ctx, d.MAC, proto.ActionReboot)
		if err != nil {
			s.Log.Warn("pending reboot check failed", "mac", d.MAC, "err", err)
			continue
		}
		if pending {
			// An operator already asked for a reboot; stacking another would just
			// reboot the AP twice in a row.
			s.Log.Info("scheduled reboot skipped: one is already pending", "mac", d.MAC)
			continue
		}

		a, err := s.Store.CreateAction(ctx, d.MAC, proto.ActionReboot, nil)
		if err != nil {
			s.Log.Warn("scheduled reboot not queued", "mac", d.MAC, "err", err)
			continue
		}
		s.Store.LogEvent(ctx, "reboot_scheduled", d.MAC, a.ID)
		s.Log.Info("scheduled reboot queued", "mac", d.MAC, "id", a.ID, "next", next)
	}
}

// nextClockOccurrence returns the next controller-local HH:MM strictly after
// from. Same-day is allowed: an operator setting 03:00 at 02:59 expects the
// event in a minute, not tomorrow.
func nextClockOccurrence(from time.Time, at string) (time.Time, error) {
	h, m, err := parseClock(at)
	if err != nil {
		return time.Time{}, err
	}
	t := time.Date(from.Year(), from.Month(), from.Day(), h, m, 0, 0, from.Location())
	if !t.After(from) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// nextRebootOccurrence returns the first due time after from for the given
// frequency: a fixed weekday, a fixed month day, or (default) the legacy
// every-N-days clock occurrence.
func nextRebootOccurrence(from time.Time, days, weekday, monthday int, at string) (time.Time, error) {
	h, m, err := parseClock(at)
	if err != nil {
		return time.Time{}, err
	}
	if weekday >= 0 {
		t := time.Date(from.Year(), from.Month(), from.Day(), h, m, 0, 0, from.Location())
		t = t.AddDate(0, 0, (int(weekday)-int(t.Weekday())+7)%7)
		if !t.After(from) {
			t = t.AddDate(0, 0, 7)
		}
		return t, nil
	}
	if monthday >= 1 {
		t := time.Date(from.Year(), from.Month(), monthday, h, m, 0, 0, from.Location())
		if !t.After(from) {
			t = t.AddDate(0, 1, 0)
		}
		return t, nil
	}
	return nextClockOccurrence(from, at)
}

// advanceReboot moves a due time past now. Weekly and monthly schedules jump
// to the next matching calendar day; interval schedules move in whole periods.
// AddDate keeps the wall-clock hour across daylight-saving changes.
func advanceReboot(due time.Time, days, weekday, monthday int, now time.Time) time.Time {
	if weekday >= 0 || monthday >= 1 {
		t, err := nextRebootOccurrence(now, days, weekday, monthday, due.Format("15:04"))
		if err != nil { // unreachable for schedules already validated at save time
			t = due.AddDate(0, 0, 7)
		}
		return t
	}
	if days < 1 {
		days = 1
	}
	for !due.After(now) {
		due = due.AddDate(0, 0, days)
	}
	return due
}

func parseClock(at string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", at)
	if err != nil {
		return 0, 0, errors.New("at must be HH:MM")
	}
	return t.Hour(), t.Minute(), nil
}

var weekdayNames = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func formatSchedule(days, weekday, monthday int, at string) string {
	if days <= 0 {
		return "disabled"
	}
	switch {
	case weekday >= 0:
		return "weekly on " + weekdayNames[weekday%7] + " at " + at
	case monthday >= 1:
		return "monthly on day " + strconv.Itoa(monthday) + " at " + at
	default:
		return "every " + strconv.Itoa(days) + "d at " + at
	}
}
