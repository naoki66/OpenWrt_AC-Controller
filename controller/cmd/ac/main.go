package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// The controller image is scratch, so it has no /usr/share/zoneinfo. Embed
	// the tz database so TZ=Asia/Shanghai actually applies to the HH:MM
	// schedules (reboot and LED) instead of silently falling back to UTC.
	_ "time/tzdata"

	"github.com/naoki66/ac-controller/controller/internal/api"
	"github.com/naoki66/ac-controller/controller/internal/config"
	"github.com/naoki66/ac-controller/controller/internal/discovery"
	"github.com/naoki66/ac-controller/controller/internal/keys"
	"github.com/naoki66/ac-controller/controller/internal/provision"
	"github.com/naoki66/ac-controller/controller/internal/store"
	"github.com/naoki66/ac-controller/shared/bundle"
	"github.com/naoki66/ac-controller/shared/version"
)

func main() {
	var (
		verbose = flag.Bool("v", false, "enable debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cmd := "serve"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}

	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "version":
		os.Stdout.WriteString("ac " + version.Version + " (" + version.Commit + ", " + version.Date + ")\n")
	case "pubkey":
		err = printPubKey()
	case "verify":
		err = verifyPackage(log)
	default:
		log.Error("unknown command", "cmd", cmd, "hint", "supported: serve, version, pubkey, verify")
		os.Exit(2)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func serve(log *slog.Logger) error {
	cfg := config.Load()

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if err := st.EnsureManagementKey(ctx); err != nil {
		return fmt.Errorf("initialize management key: %w", err)
	}

	acID, err := store.LoadOrCreateID(cfg.IDFile())
	if err != nil {
		return err
	}

	key, err := keys.LoadOrCreate(cfg.DataDir)
	if err != nil {
		return err
	}

	if err := seedTemplates(ctx, st, log); err != nil {
		return err
	}

	seededAccount, err := api.EnsureSecureAdminAccount(ctx, st, cfg.AdminUser, cfg.AdminPassword)
	if err != nil {
		return err
	}
	if seededAccount {
		log.Info("configured admin password from environment", "user", cfg.AdminUser)
	}

	for _, warning := range cfg.SecretWarnings() {
		log.Warn(warning)
	}

	log.Info("starting controller",
		"version", version.Version,
		"commit", version.Commit,
		"date", version.Date,
		"ac_id", acID,
		"data_dir", cfg.DataDir,
		"pubkey", key.PublicHex(),
		"profile", cfg.Profile,
	)

	prov := &provision.Service{
		Store:          st,
		Key:            key,
		ACID:           acID,
		PackageHistory: cfg.PackageHistory,
	}

	apiSrv := &api.Server{
		Store:           st,
		ACID:            acID,
		Key:             key,
		Provision:       prov,
		SharedSecret:    cfg.SharedSecret,
		RequireSignedAP: true,
		AdminToken:      cfg.AdminToken,
		SampleInterval:  cfg.SampleInterval,
		OfflineAfter:    cfg.OfflineAfter,
		NTPServers:      cfg.NTPServers,
		Log:             log,
		Features: &api.FeatureGate{
			Profile:          cfg.Profile,
			TelemetryHistory: cfg.TelemetryHistory,
			Firmware:         cfg.FirmwareEnabled,
			PackageHistory:   cfg.PackageHistory,
			EventsMax:        cfg.EventsMax,
		},
	}
	if cfg.Profile == "openwrt" {
		log.Info("openwrt profile active",
			"telemetry_history", cfg.TelemetryHistory,
			"firmware", cfg.FirmwareEnabled,
			"package_history", cfg.PackageHistory,
			"events_max", cfg.EventsMax,
			"sample_interval", cfg.SampleInterval.String(),
			"sample_retention", cfg.SampleRetention.String())
	}
	// A secret rotated from the devices page persists in the settings table and
	// wins over the environment, so it must be back after every restart.
	if v, err := st.GetSetting(ctx, store.SettingSharedSecret); err == nil {
		apiSrv.SetSecretOverride(v)
		log.Info("shared secret override loaded from settings", "source", "settings")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if v, err := st.GetSetting(ctx, store.SettingNTPServers); err == nil {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err == nil {
			apiSrv.SetNTPOverride(list)
			log.Info("NTP server override loaded from settings", "servers", list)
		} else {
			log.Warn("stored NTP server list is invalid; using AC_NTP_SERVERS", "err", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	listener := &discovery.Listener{
		Addr:               cfg.DiscoverAddr,
		ACID:               acID,
		HTTPPort:           cfg.HTTPPort,
		AdvertiseIP:        cfg.AdvertiseIP,
		SigningKey:         key.PublicHex(),
		Log:                log,
		SecretRequiredFunc: func() bool { return true },
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("http listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		if err := listener.Run(ctx); err != nil {
			errCh <- err
		}
	}()
	go pruneSamples(ctx, st, cfg.SampleRetention, log)
	go trimEvents(ctx, st, cfg.EventsMax, log)
	go apiSrv.RunRebootScheduler(ctx)
	go apiSrv.RunLEDScheduler(ctx)

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		stop()
		shutdown(srv, log)
		return err
	}

	shutdown(srv, log)
	return nil
}

// seedTemplates installs the default template on a fresh database. It is
// wireless-only: adoption works immediately without touching layer 2, and a
// site that wants VLANs opts in through the ports page.
func seedTemplates(ctx context.Context, st *store.Store, log *slog.Logger) error {
	n, err := st.CountTemplates(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	tpl := provision.DefaultTemplate()
	if err := st.UpsertTemplate(ctx, tpl); err != nil {
		return err
	}
	log.Info("seeded default template", "id", tpl.ID)
	return nil
}

// pruneSamples keeps the telemetry tables bounded.
//
// Without it the database grows forever: two APs with 30 clients at a 60s
// interval add roughly 90k rows a day. One pass at startup cleans up whatever a
// long downtime left behind, then an hourly pass keeps it trim.
func pruneSamples(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	if retention <= 0 {
		return
	}
	prune := func() {
		n, err := st.PruneSamples(ctx, time.Now().Add(-retention).Unix())
		if err != nil {
			log.Warn("prune telemetry failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("pruned telemetry rows", "rows", n, "retention", retention.String())
		}
	}
	prune()

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

// trimEvents bounds the audit log. The events table only grows on operator
// actions, but a long-lived controller should not accumulate them forever; a
// flash-constrained profile keeps the newest few hundred.
func trimEvents(ctx context.Context, st *store.Store, keep int, log *slog.Logger) {
	if keep <= 0 {
		return
	}
	trim := func() {
		n, err := st.TrimEvents(ctx, keep)
		if err != nil {
			log.Warn("trim events failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("trimmed event rows", "rows", n, "keep", keep)
		}
	}
	trim()

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			trim()
		}
	}
}

func printPubKey() error {
	cfg := config.Load()
	key, err := keys.LoadOrCreate(cfg.DataDir)
	if err != nil {
		return err
	}
	os.Stdout.WriteString(key.PublicHex() + "\n")
	return nil
}

// verifyPackage reproduces the check an AP performs on download: verify the
// signature against the controller key, then re-hash every payload file.
func verifyPackage(log *slog.Logger) error {
	path := flag.Arg(1)
	if path == "" {
		return errors.New("usage: ac verify <package.tar.gz>")
	}
	cfg := config.Load()

	key, err := keys.Load(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("load signing key from %s: %w", cfg.DataDir, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	manifest, files, err := bundle.Extract(data, key.Public)
	if err != nil {
		return err
	}

	os.Stdout.WriteString(fmt.Sprintf("signature   ok (ed25519)\n"))
	os.Stdout.WriteString(fmt.Sprintf("rev         %d\n", manifest.Rev))
	os.Stdout.WriteString(fmt.Sprintf("device      %s\n", manifest.DeviceMAC))
	os.Stdout.WriteString(fmt.Sprintf("template    %s\n", manifest.TemplateID))
	os.Stdout.WriteString(fmt.Sprintf("generated   %s\n", manifest.GeneratedAt.Format(time.RFC3339)))
	os.Stdout.WriteString(fmt.Sprintf("models      %v\n", manifest.ModelWhitelist))
	for name, body := range files {
		os.Stdout.WriteString(fmt.Sprintf("file        %s (%d bytes, sha256 %s)\n", name, len(body), manifest.Files[name][:16]))
	}
	log.Debug("package verified", "path", path)
	return nil
}

func shutdown(srv *http.Server, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
}
