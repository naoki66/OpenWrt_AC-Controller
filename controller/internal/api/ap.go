package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/controller/internal/discovery"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/proto"
	"github.com/naoki66/ac-controller/shared/secret"
)

// maxTelemetryJSON bounds the optional JSON blobs a hello may attach
// (wifi_caps, steering stats). They are a handful of fields; anything larger is
// a broken or hostile build, not a report worth storing.
const maxTelemetryJSON = 512

// handleHello is the single endpoint the AP agent talks to. It combines three
// jobs that would otherwise need three round trips:
//
//  1. registration — an AP that only ever broadcasts can be recorded here too;
//  2. reporting — the outcome of the last apply attempt;
//  3. polling — what revision the controller wants the AP to be running.
//
// Keeping it to one call means the AP's outbound traffic is one request per
// interval and the exchange is naturally idempotent.
func (s *Server) handleHello(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, errors.New("invalid or missing bearer token"))
		return
	}

	var hello proto.Hello
	// Unknown fields are tolerated here on purpose. This is the one endpoint a
	// *different* build talks to: a newer agent that adds a telemetry field must
	// not be rejected outright by an older controller, because a 400 on hello
	// stops the AP reporting anything at all. The management API stays strict.
	if err := json.NewDecoder(r.Body).Decode(&hello); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}

	mac := discovery.NormalizeMAC(hello.MAC)
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("malformed MAC address"))
		return
	}
	reportingMAC := mac

	ctx := r.Context()

	// A hello is also a sighting: keep model/firmware/IP fresh even for devices
	// that were adopted long ago and never broadcast again.
	hostname := hello.Hostname
	if hostname == "" {
		hostname = "unknown"
	}
	if err := s.Store.UpsertDiscovered(ctx, store.Device{
		MAC:      mac,
		Model:    hello.Model,
		Firmware: hello.Firmware,
		Hostname: hostname,
		IP:       clientIP(r),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// A nil list means the agent does not collect client data; an empty list is a
	// authoritative "this AP currently has no clients".
	var samples []store.ClientSample
	if hello.Clients != nil {
		clients := make([]store.Client, 0, len(hello.Clients))
		samples = make([]store.ClientSample, 0, len(hello.Clients))
		now := time.Now().Unix()
		for _, c := range hello.Clients {
			mac := discovery.NormalizeMAC(c.MAC)
			if mac == "" {
				continue
			}
			clients = append(clients, store.Client{
				MAC:              mac,
				APMAC:            reportingMAC,
				IP:               c.IP,
				SSID:             c.SSID,
				Ifname:           c.Ifname,
				Band:             c.Band,
				Signal:           c.Signal,
				RxRate:           c.RxRateMbps,
				TxRate:           c.TxRateMbps,
				ConnectedSeconds: int64(c.ConnectedSeconds),
			})
			samples = append(samples, store.ClientSample{
				TS:     now,
				APMAC:  reportingMAC,
				MAC:    mac,
				SSID:   c.SSID,
				Band:   c.Band,
				Ifname: c.Ifname,
				Signal: c.Signal,
				RxRate: c.RxRateMbps,
				TxRate: c.TxRateMbps,
			})
		}
		if err := s.Store.ReplaceClientsForAP(ctx, mac, clients); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// Same contract as clients: nil means the agent does not report 11k data,
	// an empty list means this AP currently has no BSS to advertise.
	if hello.Neighbors != nil {
		list := make([]store.BSSNeighbor, 0, len(hello.Neighbors))
		for _, n := range hello.Neighbors {
			list = append(list, store.BSSNeighbor{
				APMAC:  reportingMAC,
				Ifname: n.Ifname,
				BSSID:  discovery.NormalizeMAC(n.BSSID),
				SSID:   n.SSID,
				Report: n.Report,
			})
		}
		if err := s.Store.ReplaceNeighborsForAP(ctx, reportingMAC, list); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// Radio telemetry follows the same nil/empty contract as clients.
	var radios []store.RadioSample
	if hello.Radios != nil {
		now := time.Now().Unix()
		radios = make([]store.RadioSample, 0, len(hello.Radios))
		for _, rr := range hello.Radios {
			if rr.Radio == "" {
				continue
			}
			radios = append(radios, store.RadioSample{
				TS:          now,
				APMAC:       reportingMAC,
				Radio:       rr.Radio,
				Phy:         rr.Phy,
				Band:        rr.Band,
				Up:          rr.Up,
				Channel:     rr.Channel,
				FreqMHz:     rr.FreqMHz,
				HTMode:      rr.HTMode,
				WidthMHz:    rr.WidthMHz,
				TxPower:     rr.TxPower,
				Country:     rr.Country,
				Utilization: rr.Utilization,
				Clients:     rr.Clients,
			})
		}
		// The now-view converges on every hello, not on the sample throttle: a
		// renamed radio must disappear immediately, and the rewrite touches only
		// a handful of rows. History stays in radio_samples, gated by sampleDue.
		if err := s.Store.ReplaceRadiosForAP(ctx, reportingMAC, radios); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// Wired port state follows the same nil/empty contract as radios. It is
	// measured on the AP (sysfs), never derived from the controller's config.
	if hello.Ports != nil {
		ports := make([]store.PortState, 0, len(hello.Ports))
		for _, p := range hello.Ports {
			if p.Name == "" {
				continue
			}
			ports = append(ports, store.PortState{
				APMAC:     reportingMAC,
				Name:      p.Name,
				MAC:       discovery.NormalizeMAC(p.MAC),
				Up:        p.Up,
				SpeedMbps: p.SpeedMbps,
				Duplex:    p.Duplex,
				MTU:       p.MTU,
			})
		}
		if err := s.Store.ReplacePortsForAP(ctx, reportingMAC, ports); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// One row per station per interval, not per hello: the throttle is what
	// keeps the tables from growing at heartbeat rate. A profile without
	// telemetry history skips the write entirely; the now-view above is
	// unaffected, so the live pages keep working.
	if s.gate().TelemetryHistory && s.sampleDue(mac) {
		if err := s.Store.RecordSamples(ctx, samples, radios); err != nil {
			s.Log.Warn("telemetry sample failed", "mac", mac, "err", err)
		}
	}

	if err := s.Store.RecordHello(ctx, mac, hello.AppliedRev, hello.Uptime, hello.Steering, hello.SyncState, hello.SyncMessage); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	// Capability and steering reports are optional telemetry: a malformed or
	// oversized blob must not fail the hello, or the AP would stop receiving
	// configuration over a display problem. The size cap exists because the
	// endpoint accepts bodies from a different build.
	if hello.Caps != nil {
		if body, err := json.Marshal(hello.Caps); err != nil {
			s.Log.Warn("wifi caps not encoded", "mac", mac, "err", err)
		} else if len(body) > maxTelemetryJSON {
			s.Log.Warn("wifi caps report too large", "mac", mac, "bytes", len(body))
		} else if err := s.Store.RecordWifiCaps(ctx, mac, string(body)); err != nil {
			s.Log.Warn("wifi caps not recorded", "mac", mac, "err", err)
		}
	}
	if hello.SteeringStats != nil {
		if body, err := json.Marshal(hello.SteeringStats); err != nil {
			s.Log.Warn("steering stats not encoded", "mac", mac, "err", err)
		} else if len(body) > maxTelemetryJSON {
			s.Log.Warn("steering stats report too large", "mac", mac, "bytes", len(body))
		} else if err := s.Store.RecordSteeringStats(ctx, mac, string(body)); err != nil {
			s.Log.Warn("steering stats not recorded", "mac", mac, "err", err)
		}
	}

	// Firmware reports are a separate axis from config sync: an AP can be on the
	// right revision and the wrong build. An empty state is the steady state and
	// must not overwrite the last real report.
	if hello.FirmwareState != "" {
		if err := s.Store.RecordFirmware(ctx, mac, hello.FirmwareState, hello.FirmwareMessage); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// Action results are consumed before the pending queue is read, so an action
	// that just finished is not handed out again in the same reply.
	for _, res := range hello.Actions {
		if res.ID == "" {
			continue
		}
		if res.State != proto.ActionResultDone && res.State != proto.ActionResultFailed {
			s.Log.Warn("ignoring action result with unknown state", "mac", mac, "id", res.ID, "state", res.State)
			continue
		}
		finished, err := s.Store.CompleteAction(ctx, mac, res.ID, res.State, res.Message)
		if err != nil {
			s.Log.Warn("action result not recorded", "mac", mac, "id", res.ID, "err", err)
			continue
		}
		if finished {
			s.Store.LogEvent(ctx, "action_"+res.State, mac, res.Type+" "+res.ID+" "+res.Message)
			// A factory reset returns the AP to its pre-controller configuration,
			// so keeping it adopted would immediately re-apply the package on the
			// next hello. Releasing it makes the reset actually stick until an
			// operator adopts it again.
			if res.Type == proto.ActionFactoryReset && res.State == proto.ActionResultDone {
				if err := s.Store.Release(ctx, mac); err != nil {
					s.Log.Warn("factory reset could not release device", "mac", mac, "err", err)
				} else {
					s.Store.LogEvent(ctx, "factory_reset", mac, res.ID)
				}
			}
		}
	}

	device, err := s.Store.GetDevice(ctx, mac)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	reply := proto.HelloReply{
		ACID:           s.ACID,
		State:          device.State,
		PubKey:         s.Key.PublicHex(),
		SecretRequired: s.EffectiveSecret() != "",
		Firmware:       s.firmwareRef(r, device),
		// NTP is safe for a pending device too: a factory AP needs time before
		// adoption, and the list is independent of configuration revisions.
		NTP: s.EffectiveNTPServers(),
	}
	if device.State == store.StateAdopted && device.ConfigRev > 0 {
		reply.DesiredRev = device.ConfigRev
		reply.PackagePath = fmt.Sprintf("/api/v1/devices/%s/package?rev=%d", mac, device.ConfigRev)
	}

	// Commands are only meaningful for an adopted device; the create endpoint
	// already refuses pending ones, so this check is cheap belt and braces.
	if device.State == store.StateAdopted {
		actions, err := s.Store.PendingActions(ctx, mac, 8)
		if err != nil {
			s.Log.Warn("pending actions not loaded", "mac", mac, "err", err)
		} else if len(actions) > 0 {
			ids := make([]string, 0, len(actions))
			reply.Actions = make([]proto.HelloAction, 0, len(actions))
			for _, a := range actions {
				ids = append(ids, a.ID)
				reply.Actions = append(reply.Actions, proto.HelloAction{ID: a.ID, Type: a.Type, Args: a.Args})
			}
			if err := s.Store.MarkActionsDispatched(ctx, ids); err != nil {
				s.Log.Warn("actions not marked dispatched", "mac", mac, "err", err)
			}
		}
	}

	// Client bindings ("smart magnet") are controller-side intent, not part of
	// the wireless package, so they ride the hello reply. An empty non-nil list
	// tells the agent to clear its previous set; a failed load stays nil and
	// leaves the AP on its last known bindings.
	if device.State == store.StateAdopted {
		bindings, err := s.Store.ListClientBindings(ctx)
		if err != nil {
			s.Log.Warn("client bindings not loaded", "mac", mac, "err", err)
		} else {
			var neighbors []store.BSSNeighbor
			if len(bindings) > 0 {
				neighbors, err = s.Store.AllNeighbors(ctx)
				if err != nil {
					s.Log.Warn("neighbors unavailable for bindings", "err", err)
				}
			}
			reply.Bindings = make([]proto.ClientBinding, 0, len(bindings))
			for _, m := range bindings {
				b := proto.ClientBinding{MAC: m.MAC, SSID: m.BindSSID, APMAC: m.BindAPMAC}
				b.BSSID = resolveBindingBSSID(neighbors, b)
				reply.Bindings = append(reply.Bindings, b)
			}
		}
	}

	writeJSON(w, http.StatusOK, reply)
}

// resolveBindingBSSID finds the BSSID of the BSS a binding targets. An exact
// AP match wins; without an AP preference any AP serving the SSID is enough.
// No match leaves BSSID empty, and the agent falls back to matching by SSID.
func resolveBindingBSSID(neighbors []store.BSSNeighbor, b proto.ClientBinding) string {
	var fallback string
	for _, n := range neighbors {
		if n.BSSID == "" || !strings.EqualFold(n.SSID, b.SSID) {
			continue
		}
		if b.APMAC != "" && strings.EqualFold(n.APMAC, b.APMAC) {
			return n.BSSID
		}
		if fallback == "" {
			fallback = n.BSSID
		}
	}
	if b.APMAC == "" {
		return fallback
	}
	return ""
}

// authorized enforces the AP shared secret on the AP channel.
//
// The management REST API is gated separately by AC_ADMIN_TOKEN — see
// Server.adminOnly. The two credentials are deliberately independent: an AP
// must never be able to read another device's template just because it knows
// the shared secret. The effective secret is the runtime override set through
// the management API when present, otherwise AC_SHARED_SECRET.
//
// The AP presents the derived bearer token (secret.AuthToken), never the raw
// secret, so the comparison is against the same derivation this controller
// computes from the effective secret.
func (s *Server) authorized(r *http.Request) bool {
	want := s.EffectiveSecret()
	if want == "" {
		return true
	}
	return bearerMatches(r, secret.AuthToken(want))
}

// bearerMatches compares an Authorization: Bearer header against want without
// leaking it through timing.
func bearerMatches(r *http.Request, want string) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// clientIP prefers X-Forwarded-For so an agent behind a reverse proxy still
// reports a useful address.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i > 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// handleClients lists associated stations across all APs. Signal strength and
// rates come from the AP; the controller never sees these clients itself.
func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	clients, err := s.Store.ListClients(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	online := 0
	for _, c := range clients {
		if c.Online {
			online++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"clients": clients,
		"online":  online,
		"total":   len(clients),
	})
}
