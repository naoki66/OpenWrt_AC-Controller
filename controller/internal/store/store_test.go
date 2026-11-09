package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// The devices table gained template_id and config_rev in P2. An SQLite database
// created by the P1 skeleton must keep working after an in-place upgrade, so the
// migration path is covered explicitly.
func TestMigrateUpgradesLegacyDevicesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	const legacySchema = `
CREATE TABLE devices (
	mac        TEXT PRIMARY KEY,
	model      TEXT NOT NULL DEFAULT '',
	firmware   TEXT NOT NULL DEFAULT '',
	ip         TEXT NOT NULL DEFAULT '',
	hostname   TEXT NOT NULL DEFAULT '',
	state      TEXT NOT NULL DEFAULT 'pending',
	last_seen  INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO devices (mac, model, firmware, ip, hostname, state, last_seen, created_at)
VALUES ('8c:de:52:11:22:33', 'XR1710G', '20261026', '10.0.10.250', 'ap250', 'pending', 1, 1);
`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate legacy db: %v", err)
	}
	// Migrate must be re-runnable; the server calls it on every boot.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	d, err := st.GetDevice(ctx, "8c:de:52:11:22:33")
	if err != nil {
		t.Fatalf("get legacy device: %v", err)
	}
	if d.Hostname != "ap250" {
		t.Errorf("hostname = %q, want ap250", d.Hostname)
	}
	if d.TemplateID != "" || d.ConfigRev != 0 {
		t.Errorf("new columns not defaulted: %+v", d)
	}

	// The upgraded row must still be adoptable.
	if err := st.MarkAdopted(ctx, "8c:de:52:11:22:33", "default", 1); err != nil {
		t.Fatalf("adopt legacy device: %v", err)
	}
	d, err = st.GetDevice(ctx, "8c:de:52:11:22:33")
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if d.State != StateAdopted || d.ConfigRev != 1 {
		t.Errorf("after adopt: %+v", d)
	}
}

func TestPackageRevisionsAreImmutable(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ac.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := st.CreatePackage(ctx, "8c:de:52:11:22:33", 1, "aa", []byte("first")); err != nil {
		t.Fatalf("create rev 1: %v", err)
	}
	if err := st.CreatePackage(ctx, "8c:de:52:11:22:33", 1, "bb", []byte("second")); err == nil {
		t.Fatal("overwriting an existing revision was allowed")
	}

	pkg, blob, err := st.LatestPackage(ctx, "8c:de:52:11:22:33")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if pkg.Rev != 1 || string(blob) != "first" {
		t.Errorf("latest package = %+v %q", pkg, blob)
	}

	if err := st.CreatePackage(ctx, "8c:de:52:11:22:33", 2, "cc", []byte("third")); err != nil {
		t.Fatalf("create rev 2: %v", err)
	}
	pkg, _, err = st.LatestPackage(ctx, "8c:de:52:11:22:33")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if pkg.Rev != 2 {
		t.Errorf("latest rev = %d, want 2", pkg.Rev)
	}

	if _, _, err := st.PackageByRev(ctx, "8c:de:52:11:22:33", 9); err != ErrNotFound {
		t.Errorf("missing rev error = %v, want ErrNotFound", err)
	}
}

// Port state is a "now" view: a port missing from the next report disappears
// immediately instead of lingering until some retention pass.
func TestReplacePortsForAPConverges(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ports.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const ap = "11:22:33:44:55:66"
	ports := []PortState{
		{Name: "lan1", MAC: "02:00:00:00:00:01", Up: true, SpeedMbps: 2500, Duplex: "full", MTU: 1500},
		{Name: "lan2", Up: false, MTU: 1500},
	}
	if err := st.ReplacePortsForAP(ctx, ap, ports); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := st.LatestPorts(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(got) != 2 || got[0].Name != "lan1" || !got[0].Up || got[1].Name != "lan2" {
		t.Errorf("ports = %+v", got)
	}

	// A second report that drops lan2 must leave only lan1 behind.
	if err := st.ReplacePortsForAP(ctx, ap, ports[:1]); err != nil {
		t.Fatalf("replace again: %v", err)
	}
	got, err = st.LatestPorts(ctx)
	if err != nil {
		t.Fatalf("latest after converge: %v", err)
	}
	if len(got) != 1 || got[0].Name != "lan1" {
		t.Errorf("ports after converge = %+v", got)
	}
}

// A station that reappears on another AP is a roam: the row keeps the old AP
// and association start, and the counter advances exactly once per move.
func TestReplaceClientsForAPTracksRoams(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "clients.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	const (
		client = "aa:bb:cc:dd:ee:01"
		apA    = "11:22:33:44:55:66"
		apB    = "11:22:33:44:55:77"
	)
	report := func(ap string, connected int64) {
		t.Helper()
		if err := st.ReplaceClientsForAP(ctx, ap, []Client{{
			MAC: client, ConnectedSeconds: connected,
		}}); err != nil {
			t.Fatalf("report on %s: %v", ap, err)
		}
	}

	report(apA, 300)
	c, err := st.GetClient(ctx, client)
	if err != nil {
		t.Fatalf("get after first report: %v", err)
	}
	if c.APMAC != apA || c.RoamCount != 0 || c.PrevAPMAC != "" {
		t.Fatalf("first association = %+v", c)
	}

	// The same AP reporting again must not look like a roam.
	report(apA, 900)
	c, err = st.GetClient(ctx, client)
	if err != nil {
		t.Fatalf("get after repeat: %v", err)
	}
	if c.RoamCount != 0 {
		t.Errorf("roam count after repeat = %d, want 0", c.RoamCount)
	}

	report(apB, 20)
	c, err = st.GetClient(ctx, client)
	if err != nil {
		t.Fatalf("get after roam: %v", err)
	}
	if c.APMAC != apB || c.PrevAPMAC != apA || c.RoamCount != 1 {
		t.Errorf("after roaming to B: %+v", c)
	}
	if c.PrevConnectedSince == 0 {
		t.Error("previous association start was not recorded")
	}

	report(apA, 10)
	c, err = st.GetClient(ctx, client)
	if err != nil {
		t.Fatalf("get after roam back: %v", err)
	}
	if c.APMAC != apA || c.PrevAPMAC != apB || c.RoamCount != 2 {
		t.Errorf("after roaming back to A: %+v", c)
	}
}
