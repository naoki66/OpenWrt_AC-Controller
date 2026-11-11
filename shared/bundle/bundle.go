// Package bundle builds and verifies the signed configuration package an AP
// pulls before applying a revision.
//
// Layout (design §5 D3):
//
//	acpkg-<rev>.tar.gz
//	├── manifest.json   rev / target MAC / model whitelist / per-file sha256
//	├── wireless        complete /etc/config/wireless (ac_ prefixed sections)
//	└── network         fragment, only ac_ related device + interface sections
//
// The signature covers manifest.json with its own signature field removed; the
// manifest in turn covers every payload file by sha256. Verifying therefore
// needs nothing but the controller's public key.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// ManifestName is the only file the AP reads before deciding anything else.
const ManifestName = "manifest.json"

// ErrSignature reports a package whose signature does not verify. Callers must
// treat this as fatal and never apply the payload.
var ErrSignature = errors.New("bundle: signature verification failed")

// Manifest describes one configuration revision.
type Manifest struct {
	Rev         int       `json:"rev"`
	DeviceMAC   string    `json:"device_mac"`
	ACID        string    `json:"ac_id"`
	TemplateID  string    `json:"template_id"`
	GeneratedAt time.Time `json:"generated_at"`
	// ModelWhitelist keeps a package from being applied to the wrong hardware.
	ModelWhitelist []string `json:"model_whitelist"`
	// Files maps package file name to its lowercase hex sha256.
	Files     map[string]string `json:"files"`
	Signature string            `json:"signature,omitempty"`
}

// Input is everything needed to build a package.
type Input struct {
	Rev         int
	DeviceMAC   string
	ACID        string
	TemplateID  string
	Models      []string
	Files       map[string][]byte
	GeneratedAt time.Time
}

// Build signs and archives a revision.
func Build(in Input, priv ed25519.PrivateKey) (Manifest, []byte, error) {
	if len(in.Files) == 0 {
		return Manifest{}, nil, errors.New("bundle: no files to package")
	}
	if in.Rev < 1 {
		return Manifest{}, nil, fmt.Errorf("bundle: rev must be >= 1, got %d", in.Rev)
	}

	names := make([]string, 0, len(in.Files))
	for name := range in.Files {
		if err := validName(name); err != nil {
			return Manifest{}, nil, err
		}
		names = append(names, name)
	}
	sort.Strings(names)

	m := Manifest{
		Rev:            in.Rev,
		DeviceMAC:      in.DeviceMAC,
		ACID:           in.ACID,
		TemplateID:     in.TemplateID,
		GeneratedAt:    in.GeneratedAt.UTC().Truncate(time.Second),
		ModelWhitelist: in.Models,
		Files:          make(map[string]string, len(names)),
	}
	for _, name := range names {
		m.Files[name] = hashHex(in.Files[name])
	}

	sig, err := sign(m, priv)
	if err != nil {
		return Manifest{}, nil, err
	}
	m.Signature = sig

	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("bundle: encode manifest: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	entries := append([]string{ManifestName}, names...)
	for _, name := range entries {
		var body []byte
		if name == ManifestName {
			body = append(manifestBytes, '\n')
		} else {
			body = in.Files[name]
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			ModTime:  m.GeneratedAt,
			Format:   tar.FormatPAX,
		}
		// tar cannot encode a pre-1970 mtime; PAX handles modern times but a
		// zero GeneratedAt would still produce an unreadable archive.
		if hdr.ModTime.Before(time.Unix(0, 0).UTC()) {
			hdr.ModTime = time.Unix(0, 0).UTC()
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return Manifest{}, nil, fmt.Errorf("bundle: write header %s: %w", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			return Manifest{}, nil, fmt.Errorf("bundle: write %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, nil, fmt.Errorf("bundle: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return Manifest{}, nil, fmt.Errorf("bundle: close gzip: %w", err)
	}

	return m, buf.Bytes(), nil
}

// Extract verifies the package and returns its manifest plus payload files.
// Any payload file that the manifest does not list is rejected, so an attacker
// cannot slip an unsigned apply.sh into a legitimately signed archive.
func Extract(data []byte, pub ed25519.PublicKey) (Manifest, map[string][]byte, error) {
	files, err := readTarGz(data)
	if err != nil {
		return Manifest{}, nil, err
	}

	rawManifest, ok := files[ManifestName]
	if !ok {
		return Manifest{}, nil, errors.New("bundle: manifest.json missing")
	}

	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(rawManifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, nil, fmt.Errorf("bundle: decode manifest: %w", err)
	}

	if err := verifySignature(m, pub); err != nil {
		return Manifest{}, nil, err
	}

	payload := make(map[string][]byte, len(files))
	for name, body := range files {
		if name == ManifestName {
			continue
		}
		want, listed := m.Files[name]
		if !listed {
			return Manifest{}, nil, fmt.Errorf("bundle: file %q is not covered by the manifest", name)
		}
		if got := hashHex(body); got != want {
			return Manifest{}, nil, fmt.Errorf("bundle: file %q sha256 %s, manifest says %s", name, got, want)
		}
		payload[name] = body
	}
	for name := range m.Files {
		if _, present := payload[name]; !present {
			return Manifest{}, nil, fmt.Errorf("bundle: manifest lists %q but the archive does not contain it", name)
		}
	}

	return m, payload, nil
}

// Verify checks a package and returns its manifest without decoding payload.
func Verify(data []byte, pub ed25519.PublicKey) (Manifest, error) {
	m, _, err := Extract(data, pub)
	return m, err
}

// AllowsModel reports whether the package may be applied to the given model.
// An empty whitelist means the package is model agnostic.
func (m Manifest) AllowsModel(model string) bool {
	if len(m.ModelWhitelist) == 0 {
		return true
	}
	for _, allowed := range m.ModelWhitelist {
		if strings.EqualFold(allowed, model) {
			return true
		}
	}
	return false
}

func sign(m Manifest, priv ed25519.PrivateKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("bundle: invalid private key")
	}
	msg, err := signingBytes(m)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg)), nil
}

func verifySignature(m Manifest, pub ed25519.PublicKey) error {
	if m.Signature == "" {
		return ErrSignature
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("%w: malformed signature", ErrSignature)
	}
	msg, err := signingBytes(m)
	if err != nil {
		return err
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, msg, sig) {
		return ErrSignature
	}
	return nil
}

// signingBytes is the canonical message: the manifest with its signature field
// cleared. Marshalling a struct is field-order stable and map keys are sorted by
// encoding/json, so signer and verifier always agree.
func signingBytes(m Manifest) ([]byte, error) {
	unsigned := m
	unsigned.Signature = ""
	return json.Marshal(unsigned)
}

func readTarGz(data []byte) (map[string][]byte, error) {
	const (
		maxUncompressed = 16 << 20
		maxEntry        = 8 << 20
		maxEntries      = 32
	)
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("bundle: open gzip: %w", err)
	}
	defer gz.Close()

	files := map[string][]byte{}
	bounded := &io.LimitedReader{R: gz, N: maxUncompressed + 1}
	tr := tar.NewReader(bounded)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("bundle: read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			// Symlinks, devices and directories have no business in a config
			// package and are the classic tar escape vector.
			return nil, fmt.Errorf("bundle: unsupported entry type %q for %q", hdr.Typeflag, hdr.Name)
		}
		if err := validName(hdr.Name); err != nil {
			return nil, err
		}
		if len(files) >= maxEntries || hdr.Size < 0 || hdr.Size > maxEntry || hdr.Size >= bounded.N {
			return nil, errors.New("bundle: archive exceeds extraction budget")
		}
		if _, duplicate := files[hdr.Name]; duplicate {
			return nil, fmt.Errorf("bundle: duplicate file %q", hdr.Name)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("bundle: read %s: %w", hdr.Name, err)
		}
		files[hdr.Name] = body
	}
	if bounded.N == 0 {
		return nil, errors.New("bundle: archive exceeds extraction budget")
	}
	if len(files) == 0 {
		return nil, errors.New("bundle: archive is empty")
	}
	return files, nil
}

// validName rejects absolute paths and traversal so a package can be unpacked
// with a plain os.WriteFile(filepath.Join(dest, name), ...).
func validName(name string) error {
	if name == "" {
		return errors.New("bundle: empty file name")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("bundle: file name %q must not contain a path separator", name)
	}
	if path.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("bundle: file name %q is not a plain name", name)
	}
	return nil
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
