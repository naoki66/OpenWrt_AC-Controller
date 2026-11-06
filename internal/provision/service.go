// Package provision owns the adopt → render → sign → store pipeline. It is kept
// free of HTTP so the whole flow can be tested without a server, and so the same
// code can serve a future CLI or a scheduled re-push.
package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/naoki66/ac-controller/internal/bundle"
	"github.com/naoki66/ac-controller/internal/keys"
	"github.com/naoki66/ac-controller/internal/model"
	"github.com/naoki66/ac-controller/internal/render"
	"github.com/naoki66/ac-controller/internal/store"
)

// Errors the API maps onto status codes.
var (
	ErrTemplateNotFound = errors.New("provision: template not found")
	ErrNotAdopted       = errors.New("provision: device is not adopted")
	ErrNoTemplate       = errors.New("provision: adopted device has no template")
)

// Service generates configuration packages for devices.
type Service struct {
	Store *store.Store
	Key   keys.Key
	ACID  string
	// Now is injectable so tests get reproducible manifests.
	Now func() time.Time
}

// Result describes a generated revision.
type Result struct {
	MAC      string          `json:"mac"`
	Rev      int             `json:"rev"`
	SHA256   string          `json:"sha256"`
	Size     int             `json:"size"`
	Template string          `json:"template_id"`
	Manifest bundle.Manifest `json:"manifest"`
}

// Adopt assigns a template to a device and generates revision N+1.
// Adopting an already adopted device with a different template is allowed and
// simply produces the next revision.
func (s *Service) Adopt(ctx context.Context, mac, templateID string) (Result, error) {
	if templateID == "" {
		return Result{}, fmt.Errorf("%w: template_id is required", ErrTemplateNotFound)
	}
	device, err := s.Store.GetDevice(ctx, mac)
	if err != nil {
		return Result{}, err
	}

	res, err := s.generate(ctx, device, templateID)
	if err != nil {
		return Result{}, err
	}
	if err := s.Store.MarkAdopted(ctx, mac, templateID, res.Rev); err != nil {
		return Result{}, err
	}
	s.Store.LogEvent(ctx, "adopt", mac, fmt.Sprintf("adopted template %s as rev %d", templateID, res.Rev))
	return res, nil
}

// Repush regenerates the current template without changing the adoption state.
func (s *Service) Repush(ctx context.Context, mac string) (Result, error) {
	device, err := s.Store.GetDevice(ctx, mac)
	if err != nil {
		return Result{}, err
	}
	if device.State != store.StateAdopted {
		return Result{}, ErrNotAdopted
	}
	if device.TemplateID == "" {
		return Result{}, ErrNoTemplate
	}
	return s.AssignTemplate(ctx, mac, device.TemplateID)
}

// AssignTemplate moves an adopted device onto another template and generates
// the next revision. Adoption state is preserved: the device stays managed,
// only the configuration it should run changes.
func (s *Service) AssignTemplate(ctx context.Context, mac, templateID string) (Result, error) {
	if templateID == "" {
		return Result{}, fmt.Errorf("%w: template_id is required", ErrNoTemplate)
	}
	device, err := s.Store.GetDevice(ctx, mac)
	if err != nil {
		return Result{}, err
	}
	if device.State != store.StateAdopted {
		return Result{}, ErrNotAdopted
	}

	res, err := s.generate(ctx, device, templateID)
	if err != nil {
		return Result{}, err
	}
	if err := s.Store.MarkAdopted(ctx, mac, templateID, res.Rev); err != nil {
		return Result{}, err
	}
	s.Store.LogEvent(ctx, "template", mac, fmt.Sprintf("assigned template %s as rev %d", templateID, res.Rev))
	return res, nil
}

// generate renders and signs the next revision but does not touch device state;
// callers decide whether the revision came from an adopt or a repush.
func (s *Service) generate(ctx context.Context, device store.Device, templateID string) (Result, error) {
	tpl, err := s.Store.GetTemplate(ctx, templateID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("%w: %q", ErrTemplateNotFound, templateID)
		}
		return Result{}, err
	}
	// Per-unit layer-2 values are applied before validation: the addresses and
	// port maps a device reports are part of the configuration being rendered,
	// so a template that is valid for one unit must be re-checked once its
	// overrides are in place.
	//
	// Only when the template actually manages layer 2. A wireless-only template
	// renders no network file at all — the AP keeps whatever network config it
	// was flashed with — so an override recorded for a different template is
	// neither usable nor wrong: applying it would invent bridge sections the
	// template never asked for, and validating it would block the operator from
	// going back to wireless-only on an AP that was managed end to end.
	if device.NetworkParams != "" && tpl.ManagesNetwork() {
		var ov model.DeviceNetwork
		if err := json.Unmarshal([]byte(device.NetworkParams), &ov); err != nil {
			return Result{}, fmt.Errorf("provision: device %s network_params: %w", device.MAC, err)
		}
		tpl = tpl.WithDeviceNetwork(ov)
		// A device override that names an interface the template does not
		// define would silently drop a management address. Refuse the package
		// instead: an AP that loses its address is an AP that cannot be reached
		// to fix it.
		//
		// This is also what a re-adopt into the wrong template looks like: the
		// override survives Release (it records how the box is wired, not what
		// the controller last pushed), so adopting with a template whose
		// interfaces do not match reports here rather than rendering a config
		// with no management address.
		if unknown := tpl.UnmanagedInterfaceAddrs(ov); len(unknown) > 0 {
			sort.Strings(unknown)
			return Result{}, fmt.Errorf("provision: device %s network_params address %v: no such interface in template %q "+
				"(the device still carries a layer-2 override written for another template; "+
				"adopt it with the matching template_id, or clear the override via PUT /api/v1/devices/%s/network)",
				device.MAC, unknown, templateID, device.MAC)
		}
	}
	// Per-unit radio values (channel/power/mode) are a property of this AP, not
	// of the shared template, so they are applied before validation just like
	// the layer-2 overrides above.
	if device.RadioOverrides != "" {
		var ov model.DeviceRadioOverrides
		if err := json.Unmarshal([]byte(device.RadioOverrides), &ov); err != nil {
			return Result{}, fmt.Errorf("provision: device %s radio_overrides: %w", device.MAC, err)
		}
		tpl = tpl.WithDeviceRadios(ov)
	}
	if err := tpl.Validate(); err != nil {
		return Result{}, fmt.Errorf("provision: template %q is invalid: %w", templateID, err)
	}

	opts := render.Options{Prefix: render.ACPrefix}
	wireless, err := render.Wireless(tpl, opts)
	if err != nil {
		return Result{}, fmt.Errorf("provision: render wireless: %w", err)
	}
	network, err := render.Network(tpl, opts)
	if err != nil {
		return Result{}, fmt.Errorf("provision: render network: %w", err)
	}

	rev := device.ConfigRev + 1
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}

	// Pin the package to the hardware it was generated for. An AP that never
	// reported a model gets an unrestricted package: refusing to provision it
	// would be worse than the risk of a same-family mismatch.
	var models []string
	if device.Model != "" {
		models = []string{device.Model}
	}

	files := map[string][]byte{
		render.FileWireless: wireless,
		render.FileNetwork:  network,
	}
	if steering := render.Steering(tpl); steering != nil {
		files[render.FileSteering] = steering
	}
	if policy := render.SteerPolicy(tpl); policy != nil {
		files[render.FileSteerPolicy] = policy
	}

	// 802.11k peer lists, built from what every adopted AP reported about
	// itself. Omitted entirely when this AP has no siblings yet: the agent then
	// leaves hostapd alone instead of writing an empty list over nothing.
	peers, err := s.Store.AllNeighbors(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("provision: read neighbours: %w", err)
	}
	entries := make([]render.NeighborEntry, 0, len(peers))
	for _, p := range peers {
		entries = append(entries, render.NeighborEntry{
			APMAC: p.APMAC, Ifname: p.Ifname, BSSID: p.BSSID, SSID: p.SSID, Report: p.Report,
		})
	}
	if nb := render.Neighbors(entries, device.MAC); nb != nil {
		files[render.FileNeighbors] = nb
	}

	manifest, blob, err := bundle.Build(bundle.Input{
		Rev:         rev,
		DeviceMAC:   device.MAC,
		ACID:        s.ACID,
		TemplateID:  templateID,
		Models:      models,
		GeneratedAt: now(),
		Files:       files,
	}, s.Key.Private)
	if err != nil {
		return Result{}, err
	}

	sum := sha256.Sum256(blob)
	digest := hex.EncodeToString(sum[:])
	if err := s.Store.CreatePackage(ctx, device.MAC, rev, digest, blob); err != nil {
		return Result{}, err
	}

	return Result{
		MAC:      device.MAC,
		Rev:      rev,
		SHA256:   digest,
		Size:     len(blob),
		Template: templateID,
		Manifest: manifest,
	}, nil
}

// Package fetches a stored revision. rev 0 means the newest one.
func (s *Service) Package(ctx context.Context, mac string, rev int) (store.Package, []byte, error) {
	if rev == 0 {
		return s.Store.LatestPackage(ctx, mac)
	}
	return s.Store.PackageByRev(ctx, mac, rev)
}

// DefaultTemplate is the placeholder seeded on first boot so that adopt can be
// exercised before the real template exists. P1 replaces it with the golden
// baseline derived from the deployed APs (see scripts/uci-to-template.py).
func DefaultTemplate() model.Template {
	enabled := true
	return model.Template{
		ID:   "default",
		Name: "示例模板（待用现网黄金基线替换）",
		Bridge: model.Bridge{
			Name:  "br-lan",
			VLANs: []model.BridgeVLAN{{VLAN: 10, Ports: []string{"wan:t"}}},
			Interfaces: []model.Interface{{
				Name:   "ac_vlan10",
				Device: "br-lan.10",
				Proto:  "none",
			}},
		},
		Radios: []model.Radio{
			{Index: 0, Band: model.Band2G, Channel: "6", HTMode: "HE20", TxPower: 20},
			{Index: 1, Band: model.Band5G, Channel: "36", HTMode: "EHT160", TxPower: 23},
			{Index: 2, Band: model.Band6G, Channel: "37", HTMode: "EHT320", TxPower: 23},
		},
		SSIDs: []model.SSID{{
			SSID:     "AC-Test",
			Bands:    []string{model.Band2G, model.Band5G, model.Band6G},
			Network:  "ac_vlan10",
			Security: model.SecWPA2WPA3,
			PSK:      "change-this-passphrase",
			ProxyARP: true,
			Roaming: model.Roaming{
				K: true, V: true, R: true,
				MobilityDomain: "a1b2",
				FTOverDS:       &enabled,
			},
		}},
	}
}
