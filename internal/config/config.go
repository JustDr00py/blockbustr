// Package config loads blockbustr configuration from YAML with environment
// variable overrides (DESIGN §10).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for the blockbustr server.
type Config struct {
	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Redis     Redis     `yaml:"redis"`
	Paths     Paths     `yaml:"paths"`
	Compat    Compat    `yaml:"compat"`
	Transcode Transcode `yaml:"transcode"`
	Metadata  Metadata  `yaml:"metadata"`
	Debrid    Debrid    `yaml:"debrid"`
	Log       Log       `yaml:"log"`
	Libraries []Library `yaml:"libraries"`
	Scan      Scan      `yaml:"scan"`
}

// Library is one media library (DESIGN §6). Libraries are matched to the
// database by name, so renaming one creates a new library.
type Library struct {
	Name  string   `yaml:"name"`
	Kind  string   `yaml:"kind"`  // movies | tvshows
	Paths []string `yaml:"paths"` // absolute directories
}

// Scan configures library scanning.
type Scan struct {
	OnStart      bool          `yaml:"on_start"`      // scan every library at startup
	Interval     time.Duration `yaml:"interval"`      // periodic rescans; "0s" disables
	ProbeWorkers int           `yaml:"probe_workers"` // concurrent ffprobe runs; 0 = NumCPU/2
	ProbeTimeout time.Duration `yaml:"probe_timeout"` // per local file
	// MissingGrace is how long an item may be missing from disk before it is
	// deleted, so a flaky or unmounted share doesn't wipe the library.
	MissingGrace time.Duration `yaml:"missing_grace"`
	// Watch rescans a library after its files change (inotify; network
	// shares send no events, so the periodic scan stays the fallback), once
	// they have been quiet for WatchDelay.
	Watch      bool          `yaml:"watch"`
	WatchDelay time.Duration `yaml:"watch_delay"`
}

// Server configures the HTTP listener that Jellyfin clients connect to.
type Server struct {
	// Listen address. ":8096" matches Jellyfin so clients find it by default.
	Listen string `yaml:"listen"`
	// ExternalURL is the URL clients reach blockbustr at, used when building
	// absolute URLs. Empty means "derive from the request".
	ExternalURL string `yaml:"external_url"`
	// ServerName is reported as ServerName in /System/Info.
	ServerName string `yaml:"server_name"`
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// Discovery answers clients' LAN discovery broadcasts on UDP 7359
	// (env BLOCKBUSTR_DISCOVERY). In a container, publish 7359/udp and set
	// ExternalURL: the container's own address is no use to clients.
	Discovery bool `yaml:"discovery"`
	// AdminUsername/AdminPassword seed the administrator at startup. If the
	// user exists its password is reset, which doubles as lost-password
	// recovery (as in jellybird). Prefer the env vars
	// BLOCKBUSTR_ADMIN_USERNAME / BLOCKBUSTR_ADMIN_PASSWORD.
	AdminUsername string `yaml:"admin_username"`
	AdminPassword string `yaml:"admin_password"`
}

// Database configures PostgreSQL.
type Database struct {
	// URL is a postgres:// connection string (env: BLOCKBUSTR_DATABASE_URL).
	URL string `yaml:"url"`
}

// Redis configures the cache and coordination layer.
type Redis struct {
	// URL is a redis:// or rediss:// connection string (env: BLOCKBUSTR_REDIS_URL).
	URL string `yaml:"url"`
}

// Paths configures on-disk locations.
type Paths struct {
	// Cache is the root for transcode segments and image renditions.
	Cache string `yaml:"cache"`
	// Transcode and Images default to subdirectories of Cache.
	Transcode string `yaml:"transcode"`
	Images    string `yaml:"images"`
}

// TranscodeDir is where HLS sessions write segments.
func (p Paths) TranscodeDir() string { return p.orCache(p.Transcode, "transcode") }

// ImagesDir is where resized artwork is cached.
func (p Paths) ImagesDir() string { return p.orCache(p.Images, "images") }

func (p Paths) orCache(dir, sub string) string {
	if dir != "" {
		return dir
	}
	return filepath.Join(p.Cache, sub)
}

// Compat controls how blockbustr presents itself to Jellyfin clients.
type Compat struct {
	// ReportedVersion is the Jellyfin version we claim (DESIGN §11 Q1).
	ReportedVersion string `yaml:"reported_version"`
	// ProductName is reported in /System/Info/Public (DESIGN §11 Q2).
	ProductName string `yaml:"product_name"`
	// ProxyClients / RedirectClients override the remote-stream redirect
	// rule per client (matched on the auth header's Client field, DESIGN §8.2).
	ProxyClients    []string `yaml:"proxy_clients"`
	RedirectClients []string `yaml:"redirect_clients"`
	// LegacyAuth also accepts the auth forms stock Jellyfin 12.1.0 rejects
	// (X-Emby-Authorization, X-Emby-Token, X-MediaBrowser-Token, ?api_key=,
	// "Emby" scheme), for older clients (DESIGN §3.2). Env: BLOCKBUSTR_LEGACY_AUTH.
	LegacyAuth bool `yaml:"legacy_auth"`
}

// Transcode configures ffmpeg HLS sessions.
type Transcode struct {
	// HWAccel is auto, none, qsv or vaapi.
	HWAccel        string `yaml:"hwaccel"`
	SegmentSeconds int    `yaml:"segment_seconds"`
	MaxSessions    int    `yaml:"max_sessions"`
}

// Metadata configures external metadata lookups.
type Metadata struct {
	// TMDBAPIKey is a TMDB v3 key (env: BLOCKBUSTR_TMDB_API_KEY).
	TMDBAPIKey string `yaml:"tmdb_api_key"`
	Language   string `yaml:"language"`
}

// Debrid holds debrid provider credentials.
type Debrid struct {
	RealDebridAPIKey string `yaml:"realdebrid_api_key"` // env: BLOCKBUSTR_REALDEBRID_API_KEY
	TorBoxAPIKey     string `yaml:"torbox_api_key"`     // env: BLOCKBUSTR_TORBOX_API_KEY
}

// Log configures structured logging.
type Log struct {
	// Level is debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is text or json.
	Format string `yaml:"format"`
}

// Defaults returns a Config populated with sane defaults.
func Defaults() Config {
	return Config{
		Server: Server{
			Listen:          ":8096",
			ServerName:      "blockbustr",
			ShutdownTimeout: 10 * time.Second,
			Discovery:       true,
		},
		Database: Database{URL: "postgres://blockbustr:blockbustr@localhost:5432/blockbustr?sslmode=disable"},
		Redis:    Redis{URL: "redis://localhost:6379/0"},
		Paths:    Paths{Cache: "/cache"},
		Compat: Compat{
			ReportedVersion: "12.1.0",
			ProductName:     "Jellyfin Server",
			LegacyAuth:      true,
		},
		Transcode: Transcode{HWAccel: "auto", SegmentSeconds: 3, MaxSessions: 4},
		Metadata:  Metadata{Language: "en-US"},
		Log:       Log{Level: "info", Format: "text"},
		Scan: Scan{
			OnStart: true, Interval: 6 * time.Hour, ProbeTimeout: 2 * time.Minute, MissingGrace: 24 * time.Hour,
			Watch: true, WatchDelay: 30 * time.Second,
		},
	}
}

// Load reads the YAML file at path, applies defaults, then environment
// overrides, and validates the result. A missing file is fine when the
// environment carries the configuration.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return cfg, fmt.Errorf("read config: %w", err)
		default:
			dec := yaml.NewDecoder(bytes.NewReader(data))
			dec.KnownFields(true) // catch typos instead of silently ignoring them
			if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
				return cfg, fmt.Errorf("parse config %s: %w", path, err)
			}
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

var envOverrides = []struct {
	env   string
	apply func(*Config, string)
}{
	{"BLOCKBUSTR_LISTEN", func(c *Config, v string) { c.Server.Listen = v }},
	{"BLOCKBUSTR_EXTERNAL_URL", func(c *Config, v string) { c.Server.ExternalURL = v }},
	{"BLOCKBUSTR_SERVER_NAME", func(c *Config, v string) { c.Server.ServerName = v }},
	{"BLOCKBUSTR_ADMIN_USERNAME", func(c *Config, v string) { c.Server.AdminUsername = v }},
	{"BLOCKBUSTR_ADMIN_PASSWORD", func(c *Config, v string) { c.Server.AdminPassword = v }},
	{"BLOCKBUSTR_DATABASE_URL", func(c *Config, v string) { c.Database.URL = v }},
	{"BLOCKBUSTR_REDIS_URL", func(c *Config, v string) { c.Redis.URL = v }},
	{"BLOCKBUSTR_CACHE_DIR", func(c *Config, v string) { c.Paths.Cache = v }},
	{"BLOCKBUSTR_REPORTED_VERSION", func(c *Config, v string) { c.Compat.ReportedVersion = v }},
	{"BLOCKBUSTR_HWACCEL", func(c *Config, v string) { c.Transcode.HWAccel = v }},
	{"BLOCKBUSTR_LEGACY_AUTH", func(c *Config, v string) { c.Compat.LegacyAuth = parseBool(v, c.Compat.LegacyAuth) }},
	{"BLOCKBUSTR_DISCOVERY", func(c *Config, v string) { c.Server.Discovery = parseBool(v, c.Server.Discovery) }},
	{"BLOCKBUSTR_TMDB_API_KEY", func(c *Config, v string) { c.Metadata.TMDBAPIKey = v }},
	{"BLOCKBUSTR_REALDEBRID_API_KEY", func(c *Config, v string) { c.Debrid.RealDebridAPIKey = v }},
	{"BLOCKBUSTR_TORBOX_API_KEY", func(c *Config, v string) { c.Debrid.TorBoxAPIKey = v }},
	{"BLOCKBUSTR_LOG_LEVEL", func(c *Config, v string) { c.Log.Level = v }},
	{"BLOCKBUSTR_LOG_FORMAT", func(c *Config, v string) { c.Log.Format = v }},
}

func applyEnv(cfg *Config) {
	for _, o := range envOverrides {
		if v, ok := os.LookupEnv(o.env); ok && v != "" {
			o.apply(cfg, v)
		}
	}
}

var versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Validate checks that the configuration is usable and reports every problem at once.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.Server.Listen == "" {
		add("server.listen is required")
	}
	if c.Server.ExternalURL != "" {
		if u, err := url.Parse(c.Server.ExternalURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("server.external_url must be an absolute http(s) URL")
		}
	}
	if (c.Server.AdminUsername == "") != (c.Server.AdminPassword == "") {
		add("server.admin_username and server.admin_password must be set together")
	}
	if c.Server.ShutdownTimeout <= 0 {
		add("server.shutdown_timeout must be positive")
	}
	if !hasScheme(c.Database.URL, "postgres", "postgresql") {
		add("database.url must be a postgres:// URL")
	}
	if !hasScheme(c.Redis.URL, "redis", "rediss") {
		add("redis.url must be a redis:// or rediss:// URL")
	}
	if c.Paths.Cache == "" {
		add("paths.cache is required")
	}
	if !versionRE.MatchString(c.Compat.ReportedVersion) {
		add("compat.reported_version must look like 12.1.0, got %q", c.Compat.ReportedVersion)
	}
	if c.Compat.ProductName == "" {
		add("compat.product_name is required")
	}
	if !slices.Contains([]string{"auto", "none", "qsv", "vaapi"}, c.Transcode.HWAccel) {
		add("transcode.hwaccel must be auto, none, qsv or vaapi")
	}
	if c.Transcode.SegmentSeconds < 2 || c.Transcode.SegmentSeconds > 10 {
		add("transcode.segment_seconds must be between 2 and 10")
	}
	if c.Transcode.MaxSessions < 1 {
		add("transcode.max_sessions must be at least 1")
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		add("log.level must be debug, info, warn or error")
	}
	if !slices.Contains([]string{"text", "json"}, c.Log.Format) {
		add("log.format must be text or json")
	}
	seen := map[string]bool{}
	for i, l := range c.Libraries {
		key := strings.ToLower(l.Name)
		switch {
		case l.Name == "":
			add("libraries[%d].name is required", i)
		case seen[key]:
			add("libraries[%d]: duplicate name %q", i, l.Name)
		}
		seen[key] = true
		if l.Kind != "movies" && l.Kind != "tvshows" {
			add("libraries[%d] (%s): kind must be movies or tvshows", i, l.Name)
		}
		if len(l.Paths) == 0 {
			add("libraries[%d] (%s): at least one path is required", i, l.Name)
		}
		for _, p := range l.Paths {
			if !filepath.IsAbs(p) {
				add("libraries[%d] (%s): path %q must be absolute", i, l.Name, p)
			}
		}
	}
	if c.Scan.Interval < 0 || (c.Scan.Interval > 0 && c.Scan.Interval < time.Minute) {
		add("scan.interval must be 0 (off) or at least 1m")
	}
	if c.Scan.Watch && c.Scan.WatchDelay < time.Second {
		add("scan.watch_delay must be at least 1s")
	}
	if c.Scan.ProbeWorkers < 0 || c.Scan.ProbeTimeout <= 0 || c.Scan.MissingGrace < 0 {
		add("scan.probe_workers, scan.probe_timeout and scan.missing_grace must not be negative (probe_timeout > 0)")
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return nil
}

// parseBool reads 1/0, true/false, yes/no, on/off; anything else keeps def.
func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func hasScheme(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && slices.Contains(schemes, u.Scheme)
}
