package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// TestRecordSamplesRoundTrips covers the plain path: write a batch, read the
// series back.
func TestRecordSamplesRoundTrips(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	base := time.Now().Add(-time.Hour).Unix()
	if err := st.RecordSamples(ctx,
		[]ClientSample{
			{TS: base, APMAC: "8c:de:52:11:22:33", MAC: "aa:bb:cc:dd:ee:01",
				SSID: "SSID-A_5G", Band: "5g", Ifname: "phy0.1-ap0", Signal: -50, RxRate: 1200, TxRate: 900},
			{TS: base + 60, APMAC: "8c:de:52:11:22:33", MAC: "aa:bb:cc:dd:ee:01",
				SSID: "SSID-A_5G", Band: "5g", Ifname: "phy0.1-ap0", Signal: -60, RxRate: 600, TxRate: 400},
		},
		[]RadioSample{
			{TS: base, APMAC: "8c:de:52:11:22:33", Radio: "ac_radio1", Band: "5g",
				Up: true, Channel: 36, FreqMHz: 5180, HTMode: "EHT160", WidthMHz: 160,
				TxPower: 30, Utilization: 10, Clients: 2},
		}); err != nil {
		t.Fatalf("record: %v", err)
	}

	series, err := st.ClientSeries(ctx, "aa:bb:cc:dd:ee:01", base-10, base+120, 60)
	if err != nil {
		t.Fatalf("client series: %v", err)
	}
	// Two samples 60s apart land in two buckets at a 60s bucket width.
	if len(series) != 2 {
		t.Fatalf("client series = %+v, want 2 buckets", series)
	}
	if series[0].Signal != -50 || series[1].Signal != -60 {
		t.Errorf("signals = %d/%d, want -50/-60", series[0].Signal, series[1].Signal)
	}

	radios, err := st.RadioSeries(ctx, "", base-10, base+120, 60)
	if err != nil {
		t.Fatalf("radio series: %v", err)
	}
	if len(radios) != 1 {
		t.Fatalf("radio series = %+v, want 1", radios)
	}
	if radios[0].Channel != 36 || radios[0].WidthMHz != 160 || radios[0].HTMode != "EHT160" {
		t.Errorf("radio point = %+v", radios[0])
	}
}

// Country rides the same row as the measured operating point: the radio page
// shows "配置了什么法规域" next to "实际落在哪个信道".
func TestLatestRadiosCarriesCountry(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.ReplaceRadiosForAP(ctx, "8c:de:52:11:22:33", []RadioSample{
		{Radio: "radio0", Band: "2g", Up: true, Channel: 3, HTMode: "HE20", TxPower: 28, Country: "US"},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	latest, err := st.LatestRadios(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 1 || latest[0].Country != "US" {
		t.Fatalf("latest = %+v, want country US", latest)
	}
}

// A bucket is an average for measurements, but the *last* value for state. This
// is the property that keeps a channel chart honest: averaging 36 and 149 would
// draw channel 92, which no radio has ever used.
func TestRadioSeriesKeepsLastChannelButAveragesUtilization(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Aligned to the bucket width so all three samples land in one bucket; a
	// random start second would straddle a boundary and split them.
	base := (time.Now().Unix()/300)*300 - 3600
	// Three samples inside one 5-minute bucket: ACS moved 36 -> 149 -> 36.
	for i, sample := range []RadioSample{
		{TS: base, Channel: 36, Utilization: 10, Clients: 1},
		{TS: base + 60, Channel: 149, Utilization: 20, Clients: 3},
		{TS: base + 120, Channel: 36, Utilization: 30, Clients: 2},
	} {
		sample.APMAC = "8c:de:52:11:22:33"
		sample.Radio = "ac_radio1"
		sample.Up = true
		if err := st.RecordSamples(ctx, nil, []RadioSample{sample}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	points, err := st.RadioSeries(ctx, "", base-10, base+300, 300)
	if err != nil {
		t.Fatalf("radio series: %v", err)
	}
	if len(points) != 1 {
		t.Fatalf("points = %+v, want one bucket", points)
	}
	if points[0].Channel != 36 {
		t.Errorf("channel = %d, want 36 (last value in the bucket, not the average)", points[0].Channel)
	}
	if points[0].Utilization != 20 {
		t.Errorf("utilization = %d, want 20 (average of 10/20/30)", points[0].Utilization)
	}
	if points[0].Clients != 2 {
		t.Errorf("clients = %d, want 2 (average of 1/3/2)", points[0].Clients)
	}
}

// Series must stay grouped per AP and per radio: a chart that interleaves two
// radios draws a zigzag that means nothing.
func TestRadioSeriesGroupsByAPAndRadio(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()

	if err := st.RecordSamples(ctx, nil, []RadioSample{
		{TS: base, APMAC: "aa:aa:aa:aa:aa:aa", Radio: "ac_radio0", Channel: 4, Utilization: 5, Up: true},
		{TS: base, APMAC: "aa:aa:aa:aa:aa:aa", Radio: "ac_radio1", Channel: 36, Utilization: 6, Up: true},
		{TS: base, APMAC: "bb:bb:bb:bb:bb:bb", Radio: "ac_radio1", Channel: 149, Utilization: 7, Up: true},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	all, err := st.RadioSeries(ctx, "", base-10, base+10, 60)
	if err != nil {
		t.Fatalf("radio series: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all radios = %d, want 3 (%+v)", len(all), all)
	}

	one, err := st.RadioSeries(ctx, "bb:bb:bb:bb:bb:bb", base-10, base+10, 60)
	if err != nil {
		t.Fatalf("radio series: %v", err)
	}
	if len(one) != 1 || one[0].Channel != 149 {
		t.Errorf("filtered series = %+v, want the single 149 point", one)
	}
}

// LatestRadios is the "now" view: one row per radio, and the newest reported
// set wins over anything reported before.
func TestLatestRadiosReturnsOneRowPerRadio(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.ReplaceRadiosForAP(ctx, "aa:aa:aa:aa:aa:aa", []RadioSample{
		{Radio: "ac_radio1", Channel: 36, HTMode: "EHT160", Up: true},
		{Radio: "ac_radio0", Channel: 4, HTMode: "HE20", Up: true},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// A later hello reports ac_radio1 on a new channel: the row is overwritten,
	// not duplicated.
	if err := st.ReplaceRadiosForAP(ctx, "aa:aa:aa:aa:aa:aa", []RadioSample{
		{Radio: "ac_radio1", Channel: 149, HTMode: "EHT160", Up: true},
		{Radio: "ac_radio0", Channel: 4, HTMode: "HE20", Up: true},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	latest, err := st.LatestRadios(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest = %+v, want 2", latest)
	}
	if latest[0].Radio != "ac_radio0" || latest[0].Channel != 4 {
		t.Errorf("first = %+v, want ac_radio0 ch 4", latest[0])
	}
	if latest[1].Channel != 149 {
		t.Errorf("ac_radio1 channel = %d, want 149 (newest report)", latest[1].Channel)
	}
	if !latest[1].Up {
		t.Error("ac_radio1 should be up")
	}
}

// The ghost scenario from the field: UCI sections were renamed
// (ac_radio0..2 → radio0..2), and the old names kept showing up as down rows
// until retention pruning erased their samples. Convergence deletes them on
// the very hello that stops reporting them.
func TestReplaceRadiosForAPConvergesGhostRadios(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.ReplaceRadiosForAP(ctx, "aa:aa:aa:aa:aa:aa", []RadioSample{
		{Radio: "ac_radio0", Band: "2g", Up: true, Channel: 4},
		{Radio: "ac_radio1", Band: "5g", Up: true, Channel: 36},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// A second AP keeps its own rows: convergence is per AP, never global.
	if err := st.ReplaceRadiosForAP(ctx, "bb:bb:bb:bb:bb:bb", []RadioSample{
		{Radio: "ac_radio0", Band: "2g", Up: true, Channel: 1},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if err := st.ReplaceRadiosForAP(ctx, "aa:aa:aa:aa:aa:aa", []RadioSample{
		{Radio: "radio0", Band: "2g", Up: true, Channel: 4},
		{Radio: "radio1", Band: "5g", Up: true, Channel: 149},
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	latest, err := st.LatestRadios(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 3 {
		t.Fatalf("latest = %+v, want 3 rows (2 renamed + other AP)", latest)
	}
	for _, p := range latest {
		if p.APMAC == "aa:aa:aa:aa:aa:aa" && p.Radio != "radio0" && p.Radio != "radio1" {
			t.Errorf("ghost radio survived convergence: %+v", p)
		}
	}

	// An empty (but non-nil) report is authoritative too: the AP has no radios.
	if err := st.ReplaceRadiosForAP(ctx, "aa:aa:aa:aa:aa:aa", nil); err != nil {
		t.Fatalf("replace empty: %v", err)
	}
	latest, err = st.LatestRadios(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 1 || latest[0].APMAC != "bb:bb:bb:bb:bb:bb" {
		t.Fatalf("latest after clear = %+v, want only the other AP", latest)
	}
}

// Upgrading a database that predates radio_state must not blank the radio
// page: the last known sample of every radio is carried over once. The marker
// matters as much as the copy — without it every boot with an empty
// radio_state would resurrect names convergence deliberately deleted.
func TestMigrateBackfillsRadioStateOnce(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.RecordSamples(ctx, nil, []RadioSample{
		{TS: time.Now().Unix() - 120, APMAC: "aa:aa:aa:aa:aa:aa", Radio: "ac_radio1", Channel: 36, Up: true},
		{TS: time.Now().Unix() - 60, APMAC: "aa:aa:aa:aa:aa:aa", Radio: "ac_radio1", Channel: 149, Up: true},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Rewind to the pre-backfill state: no marker, empty radio_state.
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM settings WHERE key = 'radio_state_backfilled'`); err != nil {
		t.Fatalf("clear marker: %v", err)
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	latest, err := st.LatestRadios(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 1 || latest[0].Channel != 149 {
		t.Fatalf("backfilled latest = %+v, want the newest sample (ch 149)", latest)
	}

	// Second migrate with the marker set must not re-backfill, even when
	// radio_state has been emptied by convergence since.
	if _, err := st.db.ExecContext(ctx, `DELETE FROM radio_state`); err != nil {
		t.Fatalf("clear state: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	latest, err = st.LatestRadios(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 0 {
		t.Fatalf("re-backfill resurrected %+v, want empty", latest)
	}
}

// Client counts are derived from the same samples as the per-client series, so
// the overview and the detail view can never disagree.
func TestClientCountsCountDistinctStations(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	// Pin the base to a 60 s bucket boundary. ClientCounts buckets on
	// (ts/bucket)*bucket, so a base anywhere in the last ten seconds of a
	// minute puts base and base+10 in different buckets and the three samples
	// no longer share one row — a failure that depends on the wall clock, not
	// on the query.
	base := (time.Now().Add(-time.Hour).Unix() / 60) * 60

	if err := st.RecordSamples(ctx, []ClientSample{
		{TS: base, APMAC: "aa:aa:aa:aa:aa:aa", MAC: "11:11:11:11:11:11", Signal: -40},
		{TS: base + 10, APMAC: "aa:aa:aa:aa:aa:aa", MAC: "11:11:11:11:11:11", Signal: -41},
		{TS: base + 10, APMAC: "aa:aa:aa:aa:aa:aa", MAC: "22:22:22:22:22:22", Signal: -70},
		{TS: base + 10, APMAC: "bb:bb:bb:bb:bb:bb", MAC: "33:33:33:33:33:33", Signal: -55},
	}, nil); err != nil {
		t.Fatalf("record: %v", err)
	}

	counts, err := st.ClientCounts(ctx, base-10, base+60, 60)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	want := map[string]int{"aa:aa:aa:aa:aa:aa": 2, "bb:bb:bb:bb:bb:bb": 1}
	if len(counts) != len(want) {
		t.Fatalf("counts = %+v, want %v", counts, want)
	}
	for _, c := range counts {
		if c.Clients != want[c.APMAC] {
			t.Errorf("%s clients = %d, want %d", c.APMAC, c.Clients, want[c.APMAC])
		}
	}
}

// Retention is what keeps the tables bounded; an unbounded sample table is the
// difference between a 20 MB database and a 2 GB one after a month.
func TestPruneSamplesRemovesOnlyOldRows(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	now := time.Now().Unix()

	if err := st.RecordSamples(ctx,
		[]ClientSample{
			{TS: now - 10*24*3600, APMAC: "aa:aa:aa:aa:aa:aa", MAC: "11:11:11:11:11:11"},
			{TS: now - 3600, APMAC: "aa:aa:aa:aa:aa:aa", MAC: "11:11:11:11:11:11"},
		},
		[]RadioSample{
			{TS: now - 10*24*3600, APMAC: "aa:aa:aa:aa:aa:aa", Radio: "ac_radio1"},
			{TS: now - 3600, APMAC: "aa:aa:aa:aa:aa:aa", Radio: "ac_radio1"},
		}); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Retention is deliberately longer than the radio table needs; both are
	// pruned by the same age here.
	n, err := st.PruneSamples(ctx, now-7*24*3600)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned = %d, want 2", n)
	}

	clients, radios, err := st.CountSamples(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if clients != 1 || radios != 1 {
		t.Errorf("after prune: clients=%d radios=%d, want 1/1", clients, radios)
	}

	oldest, err := st.OldestSample(ctx)
	if err != nil {
		t.Fatalf("oldest: %v", err)
	}
	if oldest != now-3600 {
		t.Errorf("oldest = %d, want %d", oldest, now-3600)
	}
}

// A hello that reports no clients and no radios must not write anything, and
// must not error either — that is the steady state of an idle AP.
func TestRecordSamplesEmptyIsANoOp(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.RecordSamples(ctx, nil, nil); err != nil {
		t.Fatalf("record empty: %v", err)
	}
	clients, radios, err := st.CountSamples(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if clients != 0 || radios != 0 {
		t.Errorf("clients=%d radios=%d, want 0/0", clients, radios)
	}
	if ts, err := st.OldestSample(ctx); err != nil || ts != 0 {
		t.Errorf("oldest = %d (err %v), want 0", ts, err)
	}
}

// An AP that reports neither list must not clear existing history: nil means
// "not reported", empty means "nothing there". Same contract as the client table.
func TestClientSeriesIgnoresOtherClients(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()

	if err := st.RecordSamples(ctx, []ClientSample{
		{TS: base, APMAC: "aa:aa:aa:aa:aa:aa", MAC: "11:11:11:11:11:11", Signal: -40},
		{TS: base, APMAC: "aa:aa:aa:aa:aa:aa", MAC: "22:22:22:22:22:22", Signal: -80},
	}, nil); err != nil {
		t.Fatalf("record: %v", err)
	}
	series, err := st.ClientSeries(ctx, "11:11:11:11:11:11", base-10, base+10, 60)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(series) != 1 || series[0].Signal != -40 {
		t.Errorf("series = %+v, want only this client's -40", series)
	}
}

func TestSampleAgeIsInThePast(t *testing.T) {
	before := time.Now().Unix()
	got := SampleAge(24 * time.Hour)
	want := before - 24*3600
	if got > want+2 || got < want-2 {
		t.Errorf("SampleAge(24h) = %d, want ~%d", got, want)
	}
}
