package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naoki66/ac-controller/internal/bundle"
	"github.com/naoki66/ac-controller/internal/render"
)

// fakeRunner records commands and replays canned ubus output, so the apply
// pipeline can be exercised on a workstation without OpenWrt.
type fakeRunner struct {
	mu       sync.Mutex
	calls    []string
	statuses []string // successive `network.wireless status` outputs; last repeats
	statusN  int
	failOn   map[string]error
	// replies answers specific commands; anything unlisted falls through to the
	// empty-output default. Used by tests that need a fake ubus object to exist.
	replies map[string]string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")

	f.mu.Lock()
	f.calls = append(f.calls, key)
	err := f.failOn[key]
	f.mu.Unlock()

	if err != nil {
		return "", err
	}

	if out, ok := f.replies[key]; ok {
		return out, nil
	}

	if key == "ubus call network.wireless status" {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.statuses) == 0 {
			return "{}", nil
		}
		idx := f.statusN
		if idx >= len(f.statuses) {
			idx = len(f.statuses) - 1
		}
		f.statusN++
		return f.statuses[idx], nil
	}
	return "", nil
}

func (f *fakeRunner) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRunner) countCalls(prefix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const localWirelessFile = `config wifi-device 'radio0'
	option type 'mac80211'
	option band '2g'
	option channel '6'
	option disabled '0'

config wifi-iface 'default_radio0'
	option device 'radio0'
	option mode 'ap'
	option ssid 'LocalOnly'
	option encryption 'psk2'
	option key 'local-key'
`

const localNetworkFile = `config interface 'lan'
	option device 'br-lan'
	option proto 'static'
	option ipaddr '10.0.10.250'
	option gateway '10.0.10.1'
`

func statusWith(up bool, sections ...string) string {
	var ifaces []string
	for _, s := range sections {
		ifaces = append(ifaces, `{"section":"`+s+`","up":`+boolLit(up)+`,"config":{"ssid":"X","mode":"ap"}}`)
	}
	return `{"radio0":{"interfaces":[` + strings.Join(ifaces, ",") + `]}}`
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

type applyFixture struct {
	root    string
	applier *Applier
	runner  *fakeRunner
}

func newApplyFixture(t *testing.T, statuses ...string) *applyFixture {
	t.Helper()

	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "wireless"), []byte(localWirelessFile), 0o644); err != nil {
		t.Fatalf("seed wireless: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "network"), []byte(localNetworkFile), 0o644); err != nil {
		t.Fatalf("seed network: %v", err)
	}

	runner := &fakeRunner{statuses: statuses}
	applier := &Applier{
		ConfigDir:     configDir,
		StateDir:      filepath.Join(root, "etc", "ac"),
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 200 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
		Now:           func() time.Time { return time.Unix(1793378000, 0) },
	}
	return &applyFixture{root: root, applier: applier, runner: runner}
}

func (f *applyFixture) read(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(f.applier.ConfigDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

func packageFiles() map[string][]byte {
	return map[string][]byte{
		"wireless": []byte(`config wifi-device 'radio0'
	option type 'mac80211'
	option band '2g'
	option channel '11'
	option htmode 'HT20'
	option txpower '20'
	option disabled '0'

config wifi-iface 'ac_ssid_corp_2g'
	option device 'radio0'
	option mode 'ap'
	option ssid 'Corp'
	option encryption 'sae'
	option key 'corp-passphrase'
	option network 'ac_vlan10'
`),
		"network": []byte(`config device 'ac_br_lan_10'
	option type '8021q'
	option ifname 'br-lan'
	option vid '10'
	option name 'br-lan.10'

config interface 'ac_vlan10'
	option device 'br-lan.10'
	option proto 'none'
`),
	}
}

func TestApplyWritesMergedConfigAndVerifiesHealth(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))

	res, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, packageFiles())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Rev != 1 || len(res.Files) != 2 {
		t.Errorf("result = %+v", res)
	}
	if res.BackupDir == "" {
		t.Error("no backup directory recorded")
	}

	// The uplink must survive; that is the whole reason for section merging.
	network := f.read(t, "network")
	if !strings.Contains(network, "option ipaddr '10.0.10.250'") {
		t.Errorf("local uplink lost\n%s", network)
	}
	if !strings.Contains(network, "config interface 'ac_vlan10'") {
		t.Errorf("managed section missing\n%s", network)
	}

	// Local SSID disabled, managed SSID active.
	wireless := f.read(t, "wireless")
	if !strings.Contains(wireless, "option ssid 'LocalOnly'") {
		t.Errorf("local SSID deleted instead of disabled\n%s", wireless)
	}
	if !strings.Contains(wireless, "config wifi-iface 'ac_ssid_corp_2g'") {
		t.Errorf("managed SSID missing\n%s", wireless)
	}

	// Exactly one wifi reload and one network reload.
	if n := f.runner.countCalls("wifi reload"); n != 1 {
		t.Errorf("wifi reload called %d times, want 1 (%v)", n, f.runner.Calls())
	}
	if n := f.runner.countCalls("ubus call network reload"); n != 1 {
		t.Errorf("network reload called %d times (%v)", n, f.runner.Calls())
	}

	// Backup must contain the pre-apply content.
	backup, err := os.ReadFile(filepath.Join(res.BackupDir, "network"))
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backup) != localNetworkFile {
		t.Errorf("backup content differs from original\n%s", backup)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	ctx := context.Background()

	if _, err := f.applier.Apply(ctx, bundle.Manifest{Rev: 1}, packageFiles()); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	before := len(f.runner.Calls())

	res, err := f.applier.Apply(ctx, bundle.Manifest{Rev: 1}, packageFiles())
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(res.Files) != 0 {
		t.Errorf("second apply rewrote files: %v", res.Files)
	}
	if after := len(f.runner.Calls()); after != before {
		t.Errorf("second apply ran commands: %v", f.runner.Calls()[before:])
	}
}

func TestApplyRollsBackWhenBSSNeverComesUp(t *testing.T) {
	f := newApplyFixture(t, statusWith(false, "ac_ssid_corp_2g"))

	_, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 2}, packageFiles())
	if err == nil {
		t.Fatal("apply succeeded despite BSS being down")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Errorf("unexpected error: %v", err)
	}

	// Files restored byte for byte.
	if got := f.read(t, "network"); got != localNetworkFile {
		t.Errorf("network not restored\n--- got ---\n%s", got)
	}
	if got := f.read(t, "wireless"); got != localWirelessFile {
		t.Errorf("wireless not restored\n--- got ---\n%s", got)
	}

	// A rollback implies a second reload so the restored config takes effect.
	if n := f.runner.countCalls("wifi reload"); n != 2 {
		t.Errorf("wifi reload called %d times, want 2 (apply + rollback)", n)
	}
}

func TestApplyRollsBackWhenReloadFails(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.runner.failOn = map[string]error{"wifi reload": errors.New("wifi: not ready")}

	_, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 3}, packageFiles())
	if err == nil {
		t.Fatal("apply succeeded despite reload failure")
	}
	if got := f.read(t, "wireless"); got != localWirelessFile {
		t.Errorf("wireless not restored after reload failure\n%s", got)
	}
}

// A file that did not exist before must be removed again by the rollback,
// otherwise an AP could be left with a config fragment the operator never had.
func TestRollbackRemovesFileThatDidNotExist(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "etc", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No network file at all; only wireless exists.
	if err := os.WriteFile(filepath.Join(configDir, "wireless"), []byte(localWirelessFile), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runner := &fakeRunner{statuses: []string{statusWith(false, "ac_ssid_corp_2g")}}
	applier := &Applier{
		ConfigDir:     configDir,
		StateDir:      filepath.Join(root, "etc", "ac"),
		Runner:        runner,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		HealthTimeout: 100 * time.Millisecond,
		PollInterval:  10 * time.Millisecond,
	}
	if _, err := applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, packageFiles()); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Stat(filepath.Join(configDir, "network")); !os.IsNotExist(err) {
		t.Errorf("network file created by apply survived rollback (err=%v)", err)
	}
}

func TestDryRunDoesNotTouchConfig(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.applier.DryRun = true

	res, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 4}, packageFiles())
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(res.Staged) != 2 {
		t.Errorf("staged = %v, want 2 files", res.Staged)
	}
	if got := f.read(t, "wireless"); got != localWirelessFile {
		t.Errorf("dry run modified wireless\n%s", got)
	}
	if got := f.read(t, "network"); got != localNetworkFile {
		t.Errorf("dry run modified network\n%s", got)
	}
	if n := len(f.runner.Calls()); n != 0 {
		t.Errorf("dry run executed %d commands: %v", n, f.runner.Calls())
	}

	// The staged file must still be the correctly merged result.
	staged, err := os.ReadFile(filepath.Join(f.applier.StateDir, "staging", "rev4", "network"))
	if err != nil {
		t.Fatalf("read staged network: %v", err)
	}
	if !strings.Contains(string(staged), "ac_vlan10") || !strings.Contains(string(staged), "10.0.10.250") {
		t.Errorf("staged merge is wrong\n%s", staged)
	}
}

// A package without managed sections means the renderer produced something
// wrong; writing it would disable the AP's wifi entirely.
func TestApplyRefusesPackageWithoutACSections(t *testing.T) {
	f := newApplyFixture(t)
	files := packageFiles()
	files["wireless"] = []byte("config wifi-iface 'default_radio0'\n\toption ssid 'x'\n")

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err == nil {
		t.Fatal("apply accepted a package with no managed sections")
	}
	if got := f.read(t, "wireless"); got != localWirelessFile {
		t.Errorf("config modified despite rejection\n%s", got)
	}
	if n := len(f.runner.Calls()); n != 0 {
		t.Errorf("commands ran despite rejection: %v", f.runner.Calls())
	}
}

func TestApplyRequiresWirelessFile(t *testing.T) {
	f := newApplyFixture(t)
	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, map[string][]byte{"network": []byte("x")}); err == nil {
		t.Fatal("apply accepted a package without wireless")
	}
}

// The health check must tolerate ubus reporting radio interfaces in any order
// and with extra fields.
func TestParseWirelessStatus(t *testing.T) {
	out := `{
	  "radio0": {"interfaces": [
	    {"section": "ac_ssid_a_2g", "up": true, "config": {"ssid": "A", "mode": "ap", "extra": 1}},
	    {"section": "default_radio0", "up": false, "config": {"ssid": "Local"}}
	  ]},
	  "radio1": {"interfaces": [{"section": "ac_ssid_a_5g", "up": true, "config": {"ssid": "A"}}]}
	}`
	up, err := parseWirelessStatus(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !up["ac_ssid_a_2g"] || !up["ac_ssid_a_5g"] {
		t.Errorf("managed BSS not reported up: %v", up)
	}
	if up["default_radio0"] {
		t.Error("local BSS reported up")
	}

	if _, err := parseWirelessStatus("not json"); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := parseWirelessStatus(`{"radio0":{"interfaces":[]}}`); err == nil {
		t.Error("empty interface list accepted")
	}
}

func TestMissingBSSReport(t *testing.T) {
	missing := missingBSS([]string{"a", "b", "c"}, map[string]bool{"a": true, "b": false})
	if len(missing) != 2 {
		t.Fatalf("missing = %v", missing)
	}
	if missing[0] != "b(down)" || missing[1] != "c(absent)" {
		t.Errorf("missing = %v", missing)
	}
}

// Shape captured from a real XR1710G (ImmortalWrt 20261026): "up" sits on the
// radio, interfaces carry no up field at all. Reading a missing field as "down"
// would roll back every successful apply on real hardware.
func TestParseWirelessStatusRealShape(t *testing.T) {
	out := `{
	  "radio0": {"up": true, "disabled": false, "pending": false,
	             "interfaces": [{"section": "wifinet0", "ifname": "phy0.0-ap0",
	                             "config": {"ssid": "SSID-A"}, "stations": []}]},
	  "radio1": {"up": true, "disabled": false,
	             "interfaces": [{"section": "wifinet10", "ifname": "phy0.1-ap0"}]},
	  "radio2": {"up": false, "disabled": false,
	             "interfaces": [{"section": "wifinet20", "ifname": "phy0.2-ap0"}]}
	}`
	up, err := parseWirelessStatus(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !up["wifinet0"] || !up["wifinet10"] {
		t.Errorf("BSS on an up radio reported down: %v", up)
	}
	if up["wifinet20"] {
		t.Error("BSS on a down radio reported up")
	}

	// The health check must succeed for sections that are simply present.
	if missing := missingBSS([]string{"wifinet0", "wifinet10"}, up); len(missing) != 0 {
		t.Errorf("unexpected missing BSS: %v", missing)
	}
}

// An AP in bridged mode must not run a DHCP server: a second DHCP server on the
// segment hands out wrong gateways to the whole site (design §2.3, §7.2).
func TestApplyDisablesDHCP(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.applier.DisableDHCP = true

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, packageFiles()); err != nil {
		t.Fatalf("apply: %v", err)
	}

	calls := strings.Join(f.runner.Calls(), "\n")
	for _, want := range []string{
		"uci -q set dhcp.@dnsmasq[0].disabled=1",
		"uci -q set dhcp.odhcpd.disabled=1",
		"uci -q commit dhcp",
		"/etc/init.d/dnsmasq stop",
		"/etc/init.d/dnsmasq disable",
		"/etc/init.d/odhcpd stop",
		"/etc/init.d/odhcpd disable",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing command %q in\n%s", want, calls)
		}
	}
}

func TestApplyKeepsDHCPWhenAsked(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.applier.DisableDHCP = false

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, packageFiles()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if calls := strings.Join(f.runner.Calls(), "\n"); strings.Contains(calls, "dnsmasq") {
		t.Errorf("dnsmasq was touched despite opt-out:\n%s", calls)
	}
}

// A missing odhcpd must not abort provisioning: the wifi config is still good.
func TestDHCPFailureDoesNotFailApply(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.applier.DisableDHCP = true
	f.runner.failOn = map[string]error{
		"/etc/init.d/odhcpd stop":    errors.New("not found"),
		"/etc/init.d/odhcpd disable": errors.New("not found"),
	}

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, packageFiles()); err != nil {
		t.Fatalf("apply failed because odhcpd is absent: %v", err)
	}
}

func TestDryRunDoesNotTouchDHCP(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.applier.DryRun = true
	f.applier.DisableDHCP = true

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, packageFiles()); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if calls := strings.Join(f.runner.Calls(), "\n"); strings.Contains(calls, "dnsmasq") {
		t.Errorf("dry run touched dnsmasq:\n%s", calls)
	}
}

// The steering file is the controller's only say in client steering: it names a
// daemon, the AP runs it. Exactly one may be active.
func TestApplySteeringEnablesOneDaemon(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	files := packageFiles()
	files[render.FileSteering] = []byte("nrsyncd\n")

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err != nil {
		t.Fatalf("apply: %v", err)
	}

	calls := strings.Join(f.runner.Calls(), "\n")
	for _, want := range []string{
		"/etc/init.d/nrsyncd enable",
		"/etc/init.d/nrsyncd restart",
		// Everything else must be stopped so two daemons never fight.
		"/etc/init.d/dawn stop",
		"/etc/init.d/dawn disable",
		"/etc/init.d/usteer stop",
		"/etc/init.d/usteer disable",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in\n%s", want, calls)
		}
	}
}

// A daemon that is not installed must not fail the apply: the wifi config is
// still good, and the controller needs to see the mismatch rather than a rollback.
func TestApplySteeringToleratesMissingDaemon(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.runner.failOn = map[string]error{"/etc/init.d/nrsyncd enable": errors.New("not found")}
	files := packageFiles()
	files[render.FileSteering] = []byte("nrsyncd")

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err != nil {
		t.Fatalf("apply failed because the daemon is absent: %v", err)
	}
}

func TestApplySteeringNoneStopsEverything(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	files := packageFiles()
	files[render.FileSteering] = []byte("none\n")

	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err != nil {
		t.Fatalf("apply: %v", err)
	}
	calls := strings.Join(f.runner.Calls(), "\n")
	for _, d := range []string{"nrsyncd", "usteer", "dawn"} {
		if !strings.Contains(calls, "/etc/init.d/"+d+" disable") {
			t.Errorf("%s not disabled:/n%s", d, calls)
		}
		if strings.Contains(calls, "/etc/init.d/"+d+" enable") {
			t.Errorf("%s was enabled despite steering=none", d)
		}
	}
}

// The package has no steering file at all: the controller does not manage
// steering and the agent must not touch any daemon.
func TestApplyWithoutSteeringFileTouchesNothing(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, packageFiles()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if calls := strings.Join(f.runner.Calls(), "\n"); strings.Contains(calls, "/etc/init.d/") {
		t.Errorf("steering daemons were touched:/n%s", calls)
	}
}

func TestProbeSteering(t *testing.T) {
	f := newApplyFixture(t)
	f.runner.failOn = map[string]error{
		"/etc/init.d/nrsyncd enabled": errors.New("not installed"),
	}
	if got := f.applier.probeSteering(context.Background()); got != "usteer" {
		t.Errorf("probe = %q, want usteer (first enabled script)", got)
	}
}

// The peer lists live in hostapd memory, so a reload empties them while the UCI
// files stay identical. Applying an unchanged revision must still re-assert
// them — otherwise the feature works once and then silently stops, which is
// exactly the failure mode this whole mechanism exists to avoid.
func TestApplyRestoresNeighborsWhenConfigIsUnchanged(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.runner.replies = map[string]string{
		"ubus list":                                "hostapd.phy0.1-ap0\n",
		"ubus list hostapd.phy0.1-ap0":             "hostapd.phy0.1-ap0\n",
		"ubus call hostapd.phy0.1-ap0 rrm_nr_list": `{"list":[]}`,
	}

	files := packageFiles()
	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Second pass over the same config: no UCI change, no reload.
	files[render.FileNeighbors] = []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","Corp","00aabbccddee01"]]}`)
	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	// The fake's rrm_nr_list always reports empty, so EnsureNeighbors retries to
	// its limit here. What matters is that the write was attempted at all: the
	// UCI files were unchanged, so without this path a reload would leave the
	// list empty forever. The retry semantics are covered in neighbors_test.go.
	if n := f.runner.countCalls("ubus call hostapd.phy0.1-ap0 rrm_nr_set"); n < 1 {
		t.Error("rrm_nr_set was never called; lists must survive a reload")
	}
}

// rrm_nr_set is a device state change. An apply that finds hostapd already
// correct must not rewrite it on every cycle.
func TestApplyLeavesCorrectNeighborsAlone(t *testing.T) {
	f := newApplyFixture(t, statusWith(true, "ac_ssid_corp_2g"))
	f.runner.replies = map[string]string{
		"ubus list":                                "hostapd.phy0.1-ap0\n",
		"ubus list hostapd.phy0.1-ap0":             "hostapd.phy0.1-ap0\n",
		"ubus call hostapd.phy0.1-ap0 rrm_nr_list": `{"list":[["AA:BB:CC:DD:EE:01","Corp","00"]]}`,
	}

	files := packageFiles()
	files[render.FileNeighbors] = []byte(`{"phy0.1-ap0":[["aa:bb:cc:dd:ee:01","Corp","00"]]}`)
	if _, err := f.applier.Apply(context.Background(), bundle.Manifest{Rev: 1}, files); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if n := f.runner.countCalls("ubus call hostapd.phy0.1-ap0 rrm_nr_set"); n != 0 {
		t.Errorf("rrm_nr_set calls = %d, want 0 for an already-correct list", n)
	}
}
