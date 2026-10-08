package settings

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

func TestStore(t *testing.T) {
	q, pool := testutil.Queries(t)
	key := make([]byte, 32)
	base := config.Defaults()
	base.Explicit = map[string]string{"transcode.software": "BLOCKBUSTR_TRANSCODE_SOFTWARE"}
	s := New(base, q, key, testutil.Discard())
	if err := s.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	var seen []config.Config
	s.OnChange(func(c config.Config) { seen = append(seen, c) })
	set := func(body string) error {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		return s.Set(t.Context(), m)
	}

	if err := set(`{"stremio.redirect_hosts":[" proxy.example.com ",""],"stremio.streams.uhd_slots":1,"stremio.sync_interval":"30m","metadata.tmdb_api_key":"tmdb-secret"}`); err != nil {
		t.Fatal(err)
	}
	c := s.Config()
	if strings.Join(c.Stremio.RedirectHosts, ",") != "proxy.example.com" || c.Stremio.Streams.UHDSlots != 1 ||
		c.Stremio.SyncInterval != 30*time.Minute || c.Metadata.TMDBAPIKey != "tmdb-secret" {
		t.Errorf("effective: %+v %+v", c.Stremio, c.Metadata)
	}
	if len(seen) != 1 || seen[0].Stremio.Streams.UHDSlots != 1 {
		t.Errorf("OnChange: %d calls", len(seen))
	}
	// The secret is stored sealed, and never shown.
	var raw string
	if err := pool.QueryRow(t.Context(), `SELECT value::text FROM server_settings WHERE key = 'metadata.tmdb_api_key'`).Scan(&raw); err != nil || strings.Contains(raw, "tmdb-secret") {
		t.Errorf("stored secret %q (%v)", raw, err)
	}
	for _, v := range s.Views() {
		switch v.Key {
		case "metadata.tmdb_api_key":
			if !v.IsSet || v.Value != nil || v.Source != "admin" {
				t.Errorf("secret view: %+v", v)
			}
		case "stremio.sync_interval":
			if string(v.Value) != `"30m0s"` || v.Source != "admin" {
				t.Errorf("interval view: %+v", v)
			}
		case "transcode.software":
			if !v.Locked || v.Source != "BLOCKBUSTR_TRANSCODE_SOFTWARE" {
				t.Errorf("locked view: %+v", v)
			}
		case "stremio.streams.hd_slots":
			if v.Source != "default" || string(v.Value) != "4" {
				t.Errorf("default view: %+v", v)
			}
		}
	}

	// A new store (a restart) reads them back.
	again := New(base, q, key, testutil.Discard())
	if err := again.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := again.Config(); c.Metadata.TMDBAPIKey != "tmdb-secret" || c.Stremio.Streams.UHDSlots != 1 {
		t.Errorf("after reload: %+v", c.Stremio.Streams)
	}

	// Refused, changing nothing: locked, unknown, invalid (all or nothing).
	for body, want := range map[string]error{
		`{"transcode.software":false}`:                                  ErrLocked,
		`{"server.listen":":1"}`:                                        ErrUnknown,
		`{"stremio.streams.hd_slots":2,"stremio.streams.uhd_slots":99}`: ErrInvalid,
		`{"stremio.sync_interval":"soon"}`:                              ErrInvalid,
		`{"stremio.redirect_hosts":["https://proxy.example.com/"]}`:     ErrInvalid,
		`{"metadata.tmdb_api_key":""}`:                                  ErrInvalid,
	} {
		if err := set(body); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", body, err, want)
		}
	}
	if c := s.Config(); c.Stremio.Streams.HDSlots != 4 || c.Stremio.Streams.UHDSlots != 1 {
		t.Errorf("a refused change applied: %+v", c.Stremio.Streams)
	}

	// null resets to the default.
	if err := set(`{"stremio.streams.uhd_slots":null}`); err != nil {
		t.Fatal(err)
	}
	if c := s.Config(); c.Stremio.Streams.UHDSlots != base.Stremio.Streams.UHDSlots {
		t.Errorf("reset: %d", c.Stremio.Streams.UHDSlots)
	}

	// Without the secret key a secret can't be stored, and a stored one
	// isn't applied.
	nokey := New(base, q, nil, testutil.Discard())
	if err := nokey.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if nokey.Config().Metadata.TMDBAPIKey != "" {
		t.Error("secret applied without the key")
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"metadata.tmdb_api_key":"x"}`), &m)
	if err := nokey.Set(t.Context(), m); !errors.Is(err, ErrNoSecretKey) {
		t.Errorf("secret without the key: %v", err)
	}
}
