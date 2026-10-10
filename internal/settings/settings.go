// Package settings holds the settings an admin changes from the admin UI
// (stream proxy hosts, TMDB key, stream preferences, transcoding limits,
// catalog sync), stored in server_settings under their config keys
// ("stremio.redirect_hosts"; a secret's value is a JSON string of its
// secret.Seal'ed bytes, base64) and layered over config.yaml and the
// environment. A key either of those sets explicitly can't be changed
// here: the file or variable wins, and the UI shows the field locked.
// Changes apply at once: parts of the server that keep a setting
// subscribe with OnChange.
package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/secret"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

var (
	// ErrUnknown names a key that isn't a setting.
	ErrUnknown = errors.New("settings: no such setting")
	// ErrLocked: config.yaml or the environment sets the key.
	ErrLocked = errors.New("settings: set in config.yaml or the environment")
	// ErrInvalid: the value can't be read, or the configuration it makes
	// doesn't validate.
	ErrInvalid = errors.New("settings: invalid value")
	// ErrNoSecretKey: a secret can't be stored without BLOCKBUSTR_SECRET_KEY.
	ErrNoSecretKey = errors.New("settings: secret_key is required to store secrets (BLOCKBUSTR_SECRET_KEY)")
)

// Kinds of settings, which say how the UI edits them.
const (
	KindInt      = "int"
	KindBool     = "bool"
	KindList     = "list"     // strings
	KindDuration = "duration" // "6h", "30m"
	KindSecret   = "secret"   // a string never shown again once stored
)

// Field is one setting the admin UI offers.
type Field struct {
	Key   string // dotted, as in config.yaml
	Label string
	Help  string
	Kind  string
	ptr   func(*config.Config) any // the field in a Config
}

// Fields are the settings, in the order the UI shows them.
var Fields = []Field{
	{Key: "stremio.redirect_hosts", Label: "Stream proxy hosts", Kind: KindList,
		Help: "Hosts of stream proxies apps may fetch from themselves (MediaFlow Proxy, StremThru, an AIOStreams host's built-in proxy). Addon streams resolving to one are redirected to, so their bytes skip this server. Host names only.",
		ptr:  func(c *config.Config) any { return &c.Stremio.RedirectHosts }},
	{Key: "metadata.tmdb_api_key", Label: "TMDB API key", Kind: KindSecret,
		Help: "A TMDB v3 key, for posters, overviews and cast. Stored encrypted.",
		ptr:  func(c *config.Config) any { return &c.Metadata.TMDBAPIKey }},
	{Key: "stremio.streams.uhd_slots", Label: "4K versions offered", Kind: KindInt,
		Help: "Versions at 2160p and up offered per title (0–10).",
		ptr:  func(c *config.Config) any { return &c.Stremio.Streams.UHDSlots }},
	{Key: "stremio.streams.hd_slots", Label: "HD versions offered", Kind: KindInt,
		Help: "Versions below 2160p offered per title (0–10). Unfilled slots of either kind go to the best remaining versions.",
		ptr:  func(c *config.Config) any { return &c.Stremio.Streams.HDSlots }},
	{Key: "stremio.streams.probe_versions", Label: "Versions probed ahead", Kind: KindInt,
		Help: "Versions per title probed for their tracks before anyone plays them, best first (0 = all ready ones). Each probe reads about 10 MB through the debrid service; a version someone picks is probed then.",
		ptr:  func(c *config.Config) any { return &c.Stremio.Streams.ProbeVersions }},
	{Key: "stremio.streams.languages", Label: "Preferred audio languages", Kind: KindList,
		Help: "ISO 639-1 codes (en, de), preferred first.",
		ptr:  func(c *config.Config) any { return &c.Stremio.Streams.Languages }},
	{Key: "stremio.streams.allow_groups", Label: "Preferred release groups", Kind: KindList,
		Help: "Release groups ranked higher.",
		ptr:  func(c *config.Config) any { return &c.Stremio.Streams.AllowGroups }},
	{Key: "stremio.streams.deny_groups", Label: "Blocked release groups", Kind: KindList,
		Help: "Release groups never offered.",
		ptr:  func(c *config.Config) any { return &c.Stremio.Streams.DenyGroups }},
	{Key: "transcode.software", Label: "Allow CPU transcoding", Kind: KindBool,
		Help: "Off: video is only transcoded on a hardware encoder (no software fallback, HDR tonemapping or subtitle burn-in); anything else plays as is or not at all.",
		ptr:  func(c *config.Config) any { return &c.Transcode.Software }},
	{Key: "transcode.max_sessions", Label: "Transcodes at once", Kind: KindInt,
		Help: "Remuxes and transcodes running at the same time; one more is refused.",
		ptr:  func(c *config.Config) any { return &c.Transcode.MaxSessions }},
	{Key: "stremio.sync_interval", Label: "Catalog sync interval", Kind: KindDuration,
		Help: `How often enabled catalogs are synced ("6h", "30m"); "0s" only at start and on changes.`,
		ptr:  func(c *config.Config) any { return &c.Stremio.SyncInterval }},
	{Key: "stremio.catalog_pages", Label: "Catalog pages", Kind: KindInt,
		Help: "Pages fetched per catalog at each sync.",
		ptr:  func(c *config.Config) any { return &c.Stremio.CatalogPages }},
}

func field(key string) (Field, bool) {
	i := slices.IndexFunc(Fields, func(f Field) bool { return f.Key == key })
	if i < 0 {
		return Field{}, false
	}
	return Fields[i], true
}

// Store keeps the effective configuration: the base (defaults, config.yaml,
// environment) with the admin's stored settings on top.
type Store struct {
	q    *db.Queries
	key  []byte // seals secrets; nil: none can be stored
	log  *slog.Logger
	base config.Config

	mu     sync.Mutex // serialises Load and Set
	cur    atomic.Pointer[config.Config]
	stored map[string]json.RawMessage // as in the table (secrets sealed)
	subs   []func(config.Config)
}

// New returns a Store over base; Load reads the stored settings.
func New(base config.Config, q *db.Queries, key []byte, log *slog.Logger) *Store {
	s := &Store{q: q, key: key, log: log, base: base, stored: map[string]json.RawMessage{}}
	s.cur.Store(&base)
	return s
}

// Config is the effective configuration.
func (s *Store) Config() config.Config { return *s.cur.Load() }

// OnChange calls f with the new configuration after every change.
func (s *Store) OnChange(f func(config.Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, f)
	s.mu.Unlock()
}

// Locked says where key is set when config.yaml or the environment sets it
// ("" when it doesn't).
func (s *Store) Locked(key string) string { return s.base.Explicit[key] }

// Load reads the stored settings. One that no longer applies (now locked,
// unreadable, or making the configuration invalid) is skipped and logged.
func (s *Store) Load(ctx context.Context) error {
	keys := make([]string, len(Fields))
	for i, f := range Fields {
		keys[i] = f.Key
	}
	rows, err := s.q.ListAdminSettings(ctx, keys)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored = map[string]json.RawMessage{}
	for _, r := range rows {
		s.stored[r.Key] = r.Value
	}
	cfg, skipped := s.build(s.stored)
	for key, err := range skipped {
		s.log.Warn("stored setting not applied", "key", key, "err", err)
	}
	s.publish(cfg)
	return nil
}

// build layers stored over the base, skipping (and reporting) what doesn't
// apply.
func (s *Store) build(stored map[string]json.RawMessage) (config.Config, map[string]error) {
	cfg := s.base
	skipped := map[string]error{}
	for _, f := range Fields {
		raw, ok := stored[f.Key]
		if !ok {
			continue
		}
		if src := s.Locked(f.Key); src != "" {
			skipped[f.Key] = fmt.Errorf("%w (%s)", ErrLocked, src)
			continue
		}
		next := cfg
		if err := s.apply(f, &next, raw); err != nil {
			skipped[f.Key] = err
			continue
		}
		if err := next.Validate(); err != nil {
			skipped[f.Key] = fmt.Errorf("%w: %w", ErrInvalid, err)
			continue
		}
		cfg = next
	}
	return cfg, skipped
}

// apply decodes a stored value into f's field of c. A list is replaced,
// never written into, so configurations don't share one.
func (s *Store) apply(f Field, c *config.Config, raw json.RawMessage) error {
	switch p := f.ptr(c).(type) {
	case *time.Duration:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%w: %s wants a duration such as \"6h\"", ErrInvalid, f.Key)
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return fmt.Errorf("%w: %s wants a duration such as \"6h\"", ErrInvalid, f.Key)
		}
		*p = d
	case *string: // a secret, sealed
		var enc string
		if err := json.Unmarshal(raw, &enc); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalid, f.Key)
		}
		sealed, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalid, f.Key)
		}
		if s.key == nil {
			return ErrNoSecretKey
		}
		plain, err := secret.Open(s.key, sealed)
		if err != nil {
			return fmt.Errorf("%s can't be decrypted (secret_key changed?): %w", f.Key, err)
		}
		*p = string(plain)
	case *[]string:
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%w: %s wants a list of strings", ErrInvalid, f.Key)
		}
		*p = cleanList(v)
	default:
		if err := json.Unmarshal(raw, p); err != nil {
			return fmt.Errorf("%w: %s wants a %s", ErrInvalid, f.Key, f.Kind)
		}
	}
	return nil
}

// cleanList trims entries and drops empty ones.
func cleanList(v []string) []string {
	out := []string{}
	for _, e := range v {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Set changes settings: a value (as the UI sends it; a secret in plain
// text) is stored, null removes the stored one (back to the default). All
// or nothing: an unknown or locked key, or a value that doesn't validate,
// changes none of them.
func (s *Store) Set(ctx context.Context, changes map[string]json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := maps.Clone(s.stored)
	for key, raw := range changes {
		f, ok := field(key)
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknown, key)
		}
		if src := s.Locked(key); src != "" {
			return fmt.Errorf("%w: %s is set in %s", ErrLocked, key, src)
		}
		if len(raw) == 0 || string(raw) == "null" {
			delete(next, key)
			continue
		}
		if f.Kind == KindSecret {
			var plain string
			if err := json.Unmarshal(raw, &plain); err != nil || strings.TrimSpace(plain) == "" {
				return fmt.Errorf("%w: %s wants a non-empty string", ErrInvalid, key)
			}
			if s.key == nil {
				return ErrNoSecretKey
			}
			sealed, err := secret.Seal(s.key, []byte(strings.TrimSpace(plain)))
			if err != nil {
				return err
			}
			raw, _ = json.Marshal(base64.StdEncoding.EncodeToString(sealed))
		}
		next[key] = raw
	}
	cfg, skipped := s.build(next)
	for key := range changes {
		if err := skipped[key]; err != nil {
			return err
		}
	}
	for key := range changes {
		var err error
		if raw, ok := next[key]; ok {
			err = s.q.UpsertSetting(ctx, db.UpsertSettingParams{Key: key, Value: raw})
		} else {
			err = s.q.DeleteSetting(ctx, key)
		}
		if err != nil {
			return err
		}
	}
	s.stored = next
	s.publish(cfg)
	return nil
}

func (s *Store) publish(cfg config.Config) {
	s.cur.Store(&cfg)
	for _, f := range s.subs {
		f(cfg)
	}
}

// View is a setting as the admin UI shows it.
type View struct {
	Key, Label, Help, Kind string
	// Value is the effective value (a duration as text); never a secret's.
	Value json.RawMessage `json:",omitempty"`
	// IsSet: a secret has a value.
	IsSet bool
	// Source is where the value comes from: "default", "admin" (this page),
	// or the file or variable that sets it (then the field is locked).
	Source string
	Locked bool
}

// Views lists every setting with its effective value.
func (s *Store) Views() []View {
	s.mu.Lock()
	admin := map[string]bool{}
	for k := range s.stored {
		admin[k] = true
	}
	s.mu.Unlock()
	cfg := s.Config()
	out := make([]View, 0, len(Fields))
	for _, f := range Fields {
		v := View{Key: f.Key, Label: f.Label, Help: f.Help, Kind: f.Kind, Source: "default"}
		switch src := s.Locked(f.Key); {
		case src != "":
			v.Source, v.Locked = src, true
		case admin[f.Key]:
			v.Source = "admin"
		}
		switch p := f.ptr(&cfg).(type) {
		case *string:
			v.IsSet = *p != ""
		case *time.Duration:
			v.Value, _ = json.Marshal(p.String())
		case *[]string:
			v.Value, _ = json.Marshal(cleanList(*p))
		default:
			v.Value, _ = json.Marshal(p)
		}
		out = append(out, v)
	}
	return out
}
