package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func writePortFile(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o644); err != nil {
		t.Fatalf("write %s/%s: %v", dir, name, err)
	}
}

func TestCollectPortsReadsSysfs(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "sys/class/net")

	// sysfs exposes /sys/class/net/<if> as a symlink into /sys/devices, so the
	// test uses symlinks too: a collector that requires IsDir() sees nothing.
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", base, err)
	}
	dev1 := filepath.Join(root, "sys/devices/lan1")
	dev2 := filepath.Join(root, "sys/devices/lan2")
	for _, dir := range []string{dev1, dev2} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, link := range []string{"lan1", "lan2", "br-lan", "wlan0"} {
		target := dev1
		if link == "lan2" {
			target = dev2
		}
		if err := os.Symlink(target, filepath.Join(base, link)); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
	}
	writePortFile(t, dev1, "address", "02:00:00:00:00:01")
	writePortFile(t, dev1, "carrier", "1")
	writePortFile(t, dev1, "speed", "2500")
	writePortFile(t, dev1, "duplex", "full")
	writePortFile(t, dev1, "mtu", "1500")

	writePortFile(t, dev2, "address", "02:00:00:00:00:02")
	writePortFile(t, dev2, "carrier", "0")
	writePortFile(t, dev2, "operstate", "down")
	writePortFile(t, dev2, "speed", "-1")
	writePortFile(t, dev2, "duplex", "unknown")
	writePortFile(t, dev2, "mtu", "1500")

	ports := collectPorts(root)
	if len(ports) != 2 {
		t.Fatalf("ports = %+v, want only lan1/lan2", ports)
	}
	if ports[0].Name != "lan1" || !ports[0].Up || ports[0].SpeedMbps != 2500 ||
		ports[0].Duplex != "full" || ports[0].MTU != 1500 {
		t.Errorf("lan1 = %+v", ports[0])
	}
	if ports[1].Name != "lan2" || ports[1].Up || ports[1].SpeedMbps != 0 {
		t.Errorf("lan2 = %+v", ports[1])
	}
}

func TestCollectPortsMissingSysfsIsNil(t *testing.T) {
	if got := collectPorts(t.TempDir()); got != nil {
		t.Errorf("ports = %+v, want nil when sysfs is unreadable", got)
	}
}
