package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsValidate(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
	if cfg.Compat.ReportedVersion != "12.1.0" || cfg.Server.Listen != ":8096" {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.ServerName != "blockbustr" {
		t.Errorf("ServerName = %q", cfg.Server.ServerName)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	if _, err := Load(writeConfig(t, "")); err != nil {
		t.Fatalf("empty file should load: %v", err)
	}
}

func TestLoadYAMLAndEnv(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ":9000"
  shutdown_timeout: 3s
compat:
  proxy_clients: [Findroid]
transcode:
  hwaccel: qsv
log:
  format: json
stremio:
  streams:
    top: 5
    languages: [de, en]
    deny_groups: [YIFY]
`)
	t.Setenv("BLOCKBUSTR_LISTEN", ":9100")
	t.Setenv("BLOCKBUSTR_TMDB_API_KEY", "tmdb-key")
	t.Setenv("BLOCKBUSTR_SERVER_NAME", "") // empty env values don't override

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":9100" {
		t.Errorf("env should override yaml: listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.ShutdownTimeout != 3*time.Second || cfg.Transcode.HWAccel != "qsv" || cfg.Log.Format != "json" {
		t.Errorf("yaml not applied: %+v", cfg)
	}
	if cfg.Server.ServerName != "blockbustr" {
		t.Errorf("empty env must not override: %q", cfg.Server.ServerName)
	}
	if cfg.Metadata.TMDBAPIKey != "tmdb-key" || len(cfg.Compat.ProxyClients) != 1 {
		t.Errorf("unexpected: %+v", cfg)
	}
	if st := cfg.Stremio.Streams; st.Top != 5 || len(st.Languages) != 2 || st.DenyGroups[0] != "YIFY" || st.Timeout != 6*time.Second {
		t.Errorf("stremio.streams = %+v", st)
	}
	if cfg.Transcode.SegmentSeconds != 3 {
		t.Errorf("unset yaml keys keep defaults: segment_seconds = %d", cfg.Transcode.SegmentSeconds)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeConfig(t, "server:\n  lisen: \":1\"\n"))
	if err == nil || !strings.Contains(err.Error(), "lisen") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	cfg := Defaults()
	cfg.Database.URL = "mysql://x/y"
	cfg.Redis.URL = "localhost:6379"
	cfg.Compat.ReportedVersion = "12.1"
	cfg.Transcode.HWAccel = "nvenc"
	cfg.Server.ExternalURL = "blockbustr.local"
	cfg.Server.AdminUsername = "admin" // without a password
	cfg.Stremio.Streams.Timeout = time.Minute
	cfg.Stremio.Streams.Top = 0
	cfg.Stremio.Streams.Languages = []string{"eng"}
	cfg.Stremio.Subtitles.PerLanguage = 0
	cfg.Server.StreamURLTTL = time.Minute
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"database.url", "redis.url", "reported_version", "hwaccel", "external_url", "admin_username",
		"streams.timeout", "streams.top", "streams.languages", "subtitles.per_language", "stream_url_ttl"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestPathsDerivedFromCache(t *testing.T) {
	p := Paths{Cache: "/c"}
	if p.TranscodeDir() != "/c/transcode" || p.ImagesDir() != "/c/images" {
		t.Errorf("got %q %q", p.TranscodeDir(), p.ImagesDir())
	}
	p.Images = "/img"
	if p.ImagesDir() != "/img" {
		t.Errorf("explicit images dir ignored: %q", p.ImagesDir())
	}
}

func TestLegacyAuth(t *testing.T) {
	if !Defaults().Compat.LegacyAuth {
		t.Error("legacy auth should default to on")
	}
	cfg, err := Load(writeConfig(t, "compat:\n  legacy_auth: false\n"))
	if err != nil || cfg.Compat.LegacyAuth {
		t.Fatalf("yaml false: %v %v", cfg.Compat.LegacyAuth, err)
	}
	for v, want := range map[string]bool{"true": true, "1": true, "ON": true, "false": false, "0": false, "no": false} {
		t.Setenv("BLOCKBUSTR_LEGACY_AUTH", v)
		cfg, err := Load(writeConfig(t, "compat:\n  legacy_auth: false\n"))
		if err != nil || (cfg.Compat.LegacyAuth != want && v != "") {
			t.Errorf("env %q → %v (%v)", v, cfg.Compat.LegacyAuth, err)
		}
	}
	t.Setenv("BLOCKBUSTR_LEGACY_AUTH", "maybe")
	if cfg, _ := Load(writeConfig(t, "")); !cfg.Compat.LegacyAuth {
		t.Error("unparseable env value must keep the configured value")
	}
}

func TestLibraries(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
libraries:
  - {name: Movies, kind: movies, paths: [/media/Movies]}
  - {name: Shows, kind: tvshows, paths: [/media/Series, /mnt/more]}
scan:
  interval: 0s
  probe_workers: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Libraries) != 2 || cfg.Libraries[1].Paths[1] != "/mnt/more" || cfg.Scan.Interval != 0 || cfg.Scan.ProbeWorkers != 3 || !cfg.Scan.OnStart {
		t.Errorf("libraries/scan: %+v %+v", cfg.Libraries, cfg.Scan)
	}
	_, err = Load(writeConfig(t, `
libraries:
  - {name: Movies, kind: movies, paths: [media/Movies]}
  - {name: movies, kind: music, paths: []}
scan: {interval: 10s}
`))
	for _, want := range []string{"must be absolute", "duplicate name", "kind must be", "at least one path", "scan.interval"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
