// Command blockbustr is a media server that speaks the Jellyfin client API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/discovery"
	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/jfapi/handlers"
	"github.com/sysadmin/blockbustr/internal/library"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/metadata"
	"github.com/sysadmin/blockbustr/internal/metadata/tmdb"
	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/provider/realdebrid"
	"github.com/sysadmin/blockbustr/internal/provider/torbox"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	sqlcdb "github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
	"github.com/sysadmin/blockbustr/internal/subtitles"
	"github.com/sysadmin/blockbustr/internal/transcode"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "blockbustr:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	showVersion := flag.Bool("version", false, "print version and exit")
	healthCheck := flag.Bool("healthcheck", false, "probe the running server's /healthz and exit (for container healthchecks)")
	migrateCmd := flag.String("migrate", "", "run a database migration command (up, down, down-all, status) and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *healthCheck {
		return healthcheck(cfg.Server.Listen)
	}
	log := newLogger(os.Stdout, cfg.Log)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := pg.Open(ctx, cfg.Database.URL, 30*time.Second, log)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if *migrateCmd != "" {
		return pg.Migrate(ctx, db, *migrateCmd, log)
	}
	// Schema upgrades run on every start, like Jellyfin's own migrations.
	if err := pg.Migrate(ctx, db, pg.MigrateUp, log); err != nil {
		return err
	}
	rc, err := cache.New(ctx, cfg.Redis.URL)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	pool, err := pg.NewPool(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	queries := sqlcdb.New(pool)
	serverID, err := pg.EnsureServerID(ctx, queries)
	if err != nil {
		return err
	}
	authSvc := auth.New(queries, rc, log)
	if err := authSvc.Bootstrap(ctx, cfg.Server.AdminUsername, cfg.Server.AdminPassword); err != nil {
		return err
	}
	if err := bootstrapDebrid(ctx, queries, cfg, log); err != nil {
		return err
	}
	debrid, err := loadProviders(ctx, queries, cfg, log)
	if err != nil {
		return err
	}
	scanner := library.NewScanner(pool, rc, media.Prober{}, cfg.Scan, log)
	var tmdbClient *tmdb.Client
	if cfg.Metadata.TMDBAPIKey != "" {
		tmdbClient = tmdb.New(cfg.Metadata.TMDBAPIKey, cfg.Metadata.Language)
	} else {
		log.Info("no TMDB API key (BLOCKBUSTR_TMDB_API_KEY); metadata comes from .nfo files only")
	}
	scanner.SetMetadata(metadata.NewRefresher(pool, tmdbClient, log))
	imageStore := images.New(cfg.Paths.ImagesDir())
	scanner.AddPostScan("images", images.Prefetcher{Store: imageStore, Q: queries, Log: log}.Run)
	libs, err := scanner.SyncLibraries(ctx, cfg.Libraries)
	if err != nil {
		return err
	}

	log.Info("blockbustr starting", "version", version, "listen", cfg.Server.Listen,
		"reported_version", cfg.Compat.ReportedVersion, "server_id", dto.IDFromUUID(serverID).String())

	// Transcoding (TASKS P2.5/P2.6): test-encode each backend, pick one,
	// and reap idle ffmpeg sessions.
	hw := transcode.Probe(ctx, "")
	encoder := transcode.Choose(hw, cfg.Transcode.HWAccel)
	log.Info("transcoding", "encoder", encoder, "available", hw.Available, "device", hw.Device)
	transcoder := transcode.NewManager(cfg.Paths.TranscodeDir(), cfg.Transcode.MaxSessions)
	if err := transcoder.RemoveStale(); err != nil {
		log.Warn("could not clear old transcode sessions", "dir", cfg.Paths.TranscodeDir(), "err", err)
	}
	go transcoder.Run(ctx)

	// Live updates (TASKS P2.10): events over Redis pub/sub, delivered to
	// WebSocket clients by the hub.
	bus := &events.Bus{Cache: rc, Log: log}
	scanner.SetEvents(bus)
	hub := handlers.NewHub()
	go func() {
		if err := hub.Run(ctx, bus); err != nil {
			log.Error("websocket events unavailable", "err", err)
		}
	}()

	router := jfapi.NewRouter(log, jfapi.Options{LegacyAuth: cfg.Compat.LegacyAuth})
	addons := &stremio.Registry{Pool: pool, Client: &stremio.Client{Cache: rc}}
	if cfg.SecretKey != "" {
		if addons.Key, err = secret.ParseKey(cfg.SecretKey); err != nil {
			log.Warn("secret_key is invalid; Stremio addons can't be added or used", "err", err)
		}
	}
	handlers.Register(router, handlers.Deps{
		Config: cfg, ServerID: dto.IDFromUUID(serverID), Auth: authSvc, Queries: queries, DB: pool, Log: log, Library: scanner, Images: imageStore, Cache: rc,
		Resolver: &resolve.Resolver{Cache: rc, Providers: debrid, Log: log}, Probe: scanner,
		Transcoding: &handlers.Transcoding{Sessions: transcoder, Encoder: encoder, Device: hw.Device, SegmentSeconds: cfg.Transcode.SegmentSeconds},
		Subtitles:   &subtitles.Store{Dir: filepath.Join(cfg.Paths.Cache, "subtitles")},
		Events:      bus, Hub: hub, Addons: addons,
	})
	router.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: video streams and HLS sessions are long-lived.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// LAN discovery (TASKS P2.11): answer "who is JellyfinServer?".
	if cfg.Server.Discovery {
		_, port, _ := net.SplitHostPort(cfg.Server.Listen)
		ds, err := discovery.Listen(fmt.Sprintf(":%d", discovery.Port), discovery.Info{
			ExternalURL: cfg.Server.ExternalURL, HTTPPort: port, ID: dto.IDFromUUID(serverID).String(), Name: cfg.Server.ServerName,
		}, log)
		if err != nil {
			log.Warn("LAN discovery unavailable", "port", discovery.Port, "err", err)
		} else {
			go func() { _ = ds.Serve(ctx) }()
		}
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	// Scans run in the background so the server answers while a large
	// library is first indexed.
	go scanner.Run(ctx, libs)
	if cfg.Scan.Watch {
		go func() {
			if err := scanner.Watch(ctx, libs); err != nil {
				log.Warn("file watching unavailable; libraries are rescanned every scan.interval", "err", err)
			}
		}()
	}

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout", cfg.Server.ShutdownTimeout)
	hub.Shutdown() // tells clients, and closes the sockets srv.Shutdown wouldn't
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// bootstrapDebrid upserts the configured debrid API keys, sealed with
// AES-256-GCM under secret_key, into debrid_accounts (DESIGN §4). With no
// key configured nothing is written; removing a key leaves the row in place.
func bootstrapDebrid(ctx context.Context, q *sqlcdb.Queries, cfg config.Config, log *slog.Logger) error {
	accounts := map[string]string{
		"realdebrid": cfg.Debrid.RealDebridAPIKey,
		"torbox":     cfg.Debrid.TorBoxAPIKey,
	}
	var key []byte
	for provider, apiKey := range accounts {
		if apiKey == "" {
			continue
		}
		if key == nil {
			parsed, err := secret.ParseKey(cfg.SecretKey)
			if err != nil {
				return err
			}
			key = parsed
		}
		enc, err := secret.Seal(key, []byte(apiKey))
		if err != nil {
			return err
		}
		if err := q.UpsertDebridAccount(ctx, sqlcdb.UpsertDebridAccountParams{Provider: provider, ApiKeyEnc: enc}); err != nil {
			return err
		}
		log.Info("debrid account stored", "provider", provider)
	}
	return nil
}

// loadProviders opens the enabled debrid accounts (DESIGN §4) for the
// resolver. Stored accounts that can't be opened (secret_key removed or
// changed since they were sealed) are skipped with a warning, so playback
// of everything else still works.
func loadProviders(ctx context.Context, q *sqlcdb.Queries, cfg config.Config, log *slog.Logger) (map[provider.Name]provider.Provider, error) {
	rows, err := q.ListDebridAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := map[provider.Name]provider.Provider{}
	if len(rows) == 0 {
		return out, nil
	}
	key, kerr := secret.ParseKey(cfg.SecretKey)
	for _, row := range rows {
		if !row.Enabled {
			continue
		}
		name, err := provider.ParseName(row.Provider)
		if err != nil {
			log.Warn("debrid account skipped", "provider", row.Provider, "err", err)
			continue
		}
		if kerr != nil {
			log.Warn("debrid account skipped: secret_key missing or invalid", "provider", name)
			continue
		}
		apiKey, err := secret.Open(key, row.ApiKeyEnc)
		if err != nil {
			log.Warn("debrid account skipped: sealed under another secret_key", "provider", name)
			continue
		}
		switch name {
		case provider.RealDebrid:
			out[name] = realdebrid.New(string(apiKey), 0)
		case provider.TorBox:
			out[name] = torbox.New(string(apiKey), 0)
		}
		log.Info("debrid account enabled", "provider", name)
	}
	return out, nil
}

// healthcheck GETs /healthz on the local listener so the container image
// doesn't need curl or wget.
func healthcheck(listen string) error {
	url, err := healthURL(listen)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %s", url, resp.Status)
	}
	return nil
}

// healthURL turns a listen address like ":8096" or "0.0.0.0:8096" into a
// loopback URL that reaches it.
func healthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("healthcheck: bad listen address %q: %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func newLogger(w io.Writer, c config.Log) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(c.Level)) // validated by config.Validate
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
