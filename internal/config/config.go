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

	"github.com/sysadmin/blockbustr/internal/secret"
)

// Config is the root configuration for the blockbustr server.
type Config struct {
	Server Server `yaml:"server"`
	// SecretKey seals debrid API keys at rest (DESIGN §4 debrid_accounts):
	// exactly 32 bytes, hex or base64. Env: BLOCKBUSTR_SECRET_KEY. Required
	// only when a debrid key is configured.
	SecretKey string    `yaml:"secret_key"`
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
	Stremio   Stremio   `yaml:"stremio"`
	Search    Search    `yaml:"search"`
	Metrics   Metrics   `yaml:"metrics"`
}

// Metrics configures the Prometheus endpoint (TASKS P4.4).
type Metrics struct {
	Enabled bool   `yaml:"enabled"` // serve /metrics
	Token   string `yaml:"token"`   // when set, scrapes need "Authorization: Bearer <token>" (env BLOCKBUSTR_METRICS_TOKEN)
}

// Search configures in-client search beyond the library (DESIGN §7.4).
type Search struct {
	Enabled          bool          `yaml:"enabled"`           // search addons/Cinemeta from the clients' search
	Timeout          time.Duration `yaml:"timeout"`           // budget for one search; late sources are left out
	Retention        time.Duration `yaml:"retention"`         // found titles nobody played or favourited are dropped after this
	CinemetaFallback bool          `yaml:"cinemeta_fallback"` // search Cinemeta when no enabled addon has a search catalog
}

// Stremio configures the catalog sync (DESIGN §7.2). Addons themselves are
// managed through the admin API and stored in the database.
type Stremio struct {
	SyncInterval time.Duration `yaml:"sync_interval"` // re-sync enabled catalogs; "0s" syncs only at start and on changes
	CatalogPages int           `yaml:"catalog_pages"` // pages fetched per catalog (Cinemeta pages hold 50 titles)
	Streams      Streams       `yaml:"streams"`
	Subtitles    Subtitles     `yaml:"subtitles"`
	// RedirectHosts are stream proxies (MediaFlow Proxy, StremThru) apps
	// may fetch from themselves: an addon link resolving to one of these
	// hosts (or a subdomain) is redirected to instead of proxied, so its
	// bytes skip this server. Empty (the default): every addon stream is
	// proxied. A debrid link is never redirected to.
	RedirectHosts []string `yaml:"redirect_hosts"`
}

// Subtitles configures which addon subtitles catalog titles offer (DESIGN
// §7.3): addons like OpenSubtitles list dozens per title.
type Subtitles struct {
	Languages   []string `yaml:"languages"`    // ISO 639-1; empty = English
	PerLanguage int      `yaml:"per_language"` // tracks offered per language
}

// Streams configures stream collection and ranking at playback (DESIGN §7.3).
type Streams struct {
	Timeout     time.Duration `yaml:"timeout"`      // per addon
	UHDSlots    int           `yaml:"uhd_slots"`    // versions offered at 2160p and up
	HDSlots     int           `yaml:"hd_slots"`     // versions offered below 2160p
	Languages   []string      `yaml:"languages"`    // preferred audio, ISO 639-1 ("en", "de")
	AllowGroups []string      `yaml:"allow_groups"` // release groups ranked higher
	DenyGroups  []string      `yaml:"deny_groups"`  // release groups never offered
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
	// StreamURLTTL is how long the signed stream URLs in remote media
	// sources' Path stay valid (TASKS P3.10).
	StreamURLTTL time.Duration `yaml:"stream_url_ttl"`
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
	// Software lets the CPU transcode video: encode with libx264 when no
	// hardware encoder works, and tonemap HDR or burn in subtitles before a
	// hardware encoder. Off, only hardware transcodes run; anything else
	// plays as is or not at all.
	Software bool `yaml:"software"`
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
			StreamURLTTL:    48 * time.Hour,
		},
		Database: Database{URL: "postgres://blockbustr:blockbustr@localhost:5432/blockbustr?sslmode=disable"},
		Redis:    Redis{URL: "redis://localhost:6379/0"},
		Paths:    Paths{Cache: "/cache"},
		Compat: Compat{
			ReportedVersion: "12.1.0",
			ProductName:     "Jellyfin Server",
			LegacyAuth:      true,
		},
		Transcode: Transcode{HWAccel: "auto", SegmentSeconds: 3, MaxSessions: 4, Software: true},
		Metadata:  Metadata{Language: "en-US"},
		Log:       Log{Level: "info", Format: "text"},
		Stremio: Stremio{
			SyncInterval: 6 * time.Hour, CatalogPages: 2,
			Streams:   Streams{Timeout: 6 * time.Second, UHDSlots: 3, HDSlots: 4},
			Subtitles: Subtitles{PerLanguage: 3},
		},
		Search: Search{Enabled: true, Timeout: 3 * time.Second, Retention: 7 * 24 * time.Hour, CinemetaFallback: true},
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
	{"BLOCKBUSTR_REDIRECT_HOSTS", func(c *Config, v string) { c.Stremio.RedirectHosts = splitHosts(v) }},
	{"BLOCKBUSTR_TRANSCODE_SOFTWARE", func(c *Config, v string) { c.Transcode.Software = parseBool(v, c.Transcode.Software) }},
	{"BLOCKBUSTR_LEGACY_AUTH", func(c *Config, v string) { c.Compat.LegacyAuth = parseBool(v, c.Compat.LegacyAuth) }},
	{"BLOCKBUSTR_DISCOVERY", func(c *Config, v string) { c.Server.Discovery = parseBool(v, c.Server.Discovery) }},
	{"BLOCKBUSTR_TMDB_API_KEY", func(c *Config, v string) { c.Metadata.TMDBAPIKey = v }},
	{"BLOCKBUSTR_REALDEBRID_API_KEY", func(c *Config, v string) { c.Debrid.RealDebridAPIKey = v }},
	{"BLOCKBUSTR_TORBOX_API_KEY", func(c *Config, v string) { c.Debrid.TorBoxAPIKey = v }},
	{"BLOCKBUSTR_SECRET_KEY", func(c *Config, v string) { c.SecretKey = v }},
	{"BLOCKBUSTR_METRICS_TOKEN", func(c *Config, v string) { c.Metrics.Token = v }},
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
	if c.Stremio.SyncInterval < 0 || (c.Stremio.SyncInterval > 0 && c.Stremio.SyncInterval < 10*time.Minute) {
		add("stremio.sync_interval must be 0 (off) or at least 10m")
	}
	if c.Stremio.CatalogPages < 1 || c.Stremio.CatalogPages > 20 {
		add("stremio.catalog_pages must be between 1 and 20")
	}
	if c.Server.StreamURLTTL < time.Hour || c.Server.StreamURLTTL > 30*24*time.Hour {
		add("server.stream_url_ttl must be between 1h and 720h")
	}
	if c.Search.Timeout < 500*time.Millisecond || c.Search.Timeout > 15*time.Second {
		add("search.timeout must be between 500ms and 15s")
	}
	if c.Search.Retention < time.Hour {
		add("search.retention must be at least 1h")
	}
	if c.Stremio.Streams.Timeout < time.Second || c.Stremio.Streams.Timeout > 30*time.Second {
		add("stremio.streams.timeout must be between 1s and 30s")
	}
	if s := c.Stremio.Streams; s.UHDSlots < 0 || s.UHDSlots > 10 || s.HDSlots < 0 || s.HDSlots > 10 || s.UHDSlots+s.HDSlots < 1 {
		add("stremio.streams.uhd_slots and hd_slots must be 0-10 each, at least one above 0")
	}
	for _, h := range c.Stremio.RedirectHosts {
		if h == "" || strings.ContainsAny(h, "/:@?# ") {
			add("stremio.redirect_hosts: %q must be a host name alone (mfp.example.com), no scheme, port or path", h)
		}
	}
	for _, l := range c.Stremio.Streams.Languages {
		if len(l) != 2 {
			add("stremio.streams.languages must be ISO 639-1 codes (\"en\"), got %q", l)
		}
	}
	for _, l := range c.Stremio.Subtitles.Languages {
		if len(l) != 2 {
			add("stremio.subtitles.languages must be ISO 639-1 codes (\"en\"), got %q", l)
		}
	}
	if c.Stremio.Subtitles.PerLanguage < 1 || c.Stremio.Subtitles.PerLanguage > 10 {
		add("stremio.subtitles.per_language must be between 1 and 10")
	}
	if c.Debrid.RealDebridAPIKey != "" || c.Debrid.TorBoxAPIKey != "" {
		if c.SecretKey == "" {
			add("secret_key is required when a debrid API key is configured (32 bytes, hex or base64)")
		} else if _, err := secret.ParseKey(c.SecretKey); err != nil {
			add("secret_key must be 32 bytes, hex or base64 encoded")
		}
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

// splitHosts reads a comma- or space-separated host list
// (BLOCKBUSTR_REDIRECT_HOSTS).
func splitHosts(v string) []string {
	var out []string
	for _, h := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, h)
	}
	return out
}
