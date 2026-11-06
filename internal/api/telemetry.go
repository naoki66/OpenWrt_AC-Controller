package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/naoki66/ac-controller/internal/discovery"
	"github.com/naoki66/ac-controller/internal/store"
)

// Telemetry query defaults. Points are the target number of dots on a chart:
// the bucket size is derived from the requested range so that a 7 day view and
// a 1 hour view both come back with roughly the same amount of data.
const (
	defaultTelemetryRange = 6 * time.Hour
	telemetryPoints       = 240
	maxTelemetryRange     = 30 * 24 * time.Hour
)

// parseRange accepts "30m", "6h", "24h", "7d". Anything else is an error rather
// than a silent fallback: a typo that quietly returns an hour of data is worse
// than a 400 the operator can see.
func parseRange(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultTelemetryRange, nil
	}
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 {
		return 0, errors.New("range must look like 30m, 6h, 24h or 7d")
	}
	n, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil || n <= 0 {
		return 0, errors.New("range must look like 30m, 6h, 24h or 7d")
	}
	var unit time.Duration
	switch raw[len(raw)-1] {
	case 'm':
		unit = time.Minute
	case 'h':
		unit = time.Hour
	case 'd':
		unit = 24 * time.Hour
	default:
		return 0, errors.New("range must look like 30m, 6h, 24h or 7d")
	}
	d := time.Duration(n) * unit
	if d > maxTelemetryRange {
		return 0, errors.New("range is too long (max 30d)")
	}
	return d, nil
}

// bucketFor picks a bucket width that keeps a chart near telemetryPoints dots,
// never narrower than one minute. Radio history is sampled once a minute, so a
// finer bucket would produce empty buckets rather than more detail.
func bucketFor(d time.Duration) int {
	b := int(d.Seconds()) / telemetryPoints
	if b < 60 {
		b = 60
	}
	return b
}

// handleTelemetryClients returns one client's signal and rate history.
//
// The client MAC is required: a chart of every client at once is unreadable and
// would also be the largest query in the system.
func (s *Server) handleTelemetryClients(w http.ResponseWriter, r *http.Request) {
	mac := discovery.NormalizeMAC(r.URL.Query().Get("mac"))
	if mac == "" {
		writeError(w, http.StatusBadRequest, errors.New("mac is required"))
		return
	}
	d, err := parseRange(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	now := time.Now()
	bucket := bucketFor(d)
	points, err := s.Store.ClientSeries(r.Context(), mac, now.Add(-d).Unix(), now.Unix(), bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if points == nil {
		points = []store.ClientPoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mac":    mac,
		"range":  d.String(),
		"bucket": bucket,
		"points": points,
	})
}

// handleTelemetryRadios returns per-radio channel and utilisation history.
// ap is optional; omitting it returns every AP, which is what the overview chart
// draws.
func (s *Server) handleTelemetryRadios(w http.ResponseWriter, r *http.Request) {
	ap := ""
	if raw := r.URL.Query().Get("ap"); raw != "" {
		ap = discovery.NormalizeMAC(raw)
		if ap == "" {
			writeError(w, http.StatusBadRequest, errors.New("ap must be a MAC address"))
			return
		}
	}
	d, err := parseRange(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	now := time.Now()
	bucket := bucketFor(d)
	points, err := s.Store.RadioSeries(r.Context(), ap, now.Add(-d).Unix(), now.Unix(), bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if points == nil {
		points = []store.RadioPoint{}
	}

	oldest, err := s.Store.OldestSample(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ap":        ap,
		"range":     d.String(),
		"bucket":    bucket,
		"points":    points,
		"oldest_ts": oldest,
	})
}

// handleTelemetryCounts returns how many stations each AP carried over time.
// Derived from the same samples as the per-client series so the two can never
// disagree.
func (s *Server) handleTelemetryCounts(w http.ResponseWriter, r *http.Request) {
	d, err := parseRange(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now()
	bucket := bucketFor(d)
	points, err := s.Store.ClientCounts(r.Context(), now.Add(-d).Unix(), now.Unix(), bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if points == nil {
		points = []store.ClientCountPoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range":  d.String(),
		"bucket": bucket,
		"points": points,
	})
}

// handleRadios returns the newest sample per radio across the fleet. This is
// the "now" view for the radio page: unlike the series endpoint it is not
// bucketed, so the configured htmode and the phy name survive.
func (s *Server) handleRadios(w http.ResponseWriter, r *http.Request) {
	radios, err := s.Store.LatestRadios(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if radios == nil {
		radios = []store.RadioPoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"radios": radios})
}
