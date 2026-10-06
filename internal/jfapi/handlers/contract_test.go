package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/testutil"
)

// Contract harness (TASKS P1.11, DESIGN §9.2): replay captured client
// requests against blockbustr and compare the response *shape* with what
// Jellyfin 12.1.0 returned: same status, same object keys, same JSON value
// kinds, recursively. Values are compared only where a test asks for it.

type capture struct {
	Name     string
	Endpoint string
	Request  struct {
		Method  string              `json:"method"`
		Path    string              `json:"path"`
		Query   map[string][]string `json:"query"`
		Headers map[string]string   `json:"headers"`
		Body    json.RawMessage     `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"response"`
}

// capturesFor returns every scrubbed capture of endpoint (e.g.
// "GET /System/Info/Public"), matched case-insensitively.
func capturesFor(t *testing.T, endpoint string) []capture {
	t.Helper()
	files, _ := filepath.Glob("../../../testdata/jellyfin/*/*/[0-9]*.json")
	var out []capture
	for _, p := range files {
		if strings.Contains(p, "/_raw/") {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var c capture
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if strings.EqualFold(c.Endpoint, endpoint) {
			c.Name = filepath.Base(filepath.Dir(filepath.Dir(p))) + "/" + filepath.Base(filepath.Dir(p)) + "/" + filepath.Base(p)
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no captures for %s", endpoint)
	}
	return out
}

func newTestServer(t *testing.T) (http.Handler, Deps) {
	t.Helper()
	d := Deps{Config: config.Defaults(), ServerID: dto.IDFromUUID(uuid.MustParse("cccc0000-0000-0000-0000-000000000001"))}
	rt := jfapi.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), jfapi.Options{LegacyAuth: d.Config.Compat.LegacyAuth})
	Register(rt, d)
	return rt, d
}

// replay sends a capture's request (method, path, query, headers, body) to h.
func replay(t *testing.T, h http.Handler, c capture) *httptest.ResponseRecorder {
	t.Helper()
	target := c.Request.Path
	if len(c.Request.Query) > 0 {
		target += "?" + url.Values(c.Request.Query).Encode()
	}
	var body io.Reader
	if len(c.Request.Body) > 0 && string(c.Request.Body) != "null" {
		body = bytes.NewReader(c.Request.Body)
	}
	req := httptest.NewRequestWithContext(context.Background(), c.Request.Method, target, body)
	for k, v := range c.Request.Headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sameShape reports the first structural difference between two JSON values.
func sameShape(path string, want, got any) error { return sameShapeIgnoring(path, want, got, nil) }

var indexRE = regexp.MustCompile(`\[\d+\]`)

// sameShapeIgnoring is sameShape that skips the given paths, written with
// [*] for any array index (e.g. "$.Items[*].ImageTags"). Use it for
// differences that are expected, not bugs, and say why at the call site.
func sameShapeIgnoring(path string, want, got any, ignore map[string]bool) error {
	if ignore[indexRE.ReplaceAllString(path, "[*]")] {
		return nil
	}
	kind := func(v any) string {
		switch v.(type) {
		case map[string]any:
			return "object"
		case []any:
			return "array"
		case string:
			return "string"
		case float64:
			return "number"
		case bool:
			return "bool"
		case nil:
			return "null"
		}
		return fmt.Sprintf("%T", v)
	}
	if kind(want) != kind(got) {
		return fmt.Errorf("%s: want %s, got %s", path, kind(want), kind(got))
	}
	switch w := want.(type) {
	case map[string]any:
		g := got.(map[string]any)
		var missing, extra []string
		skip := func(k string) bool { return ignore[indexRE.ReplaceAllString(path+"."+k, "[*]")] }
		for k := range w {
			if _, ok := g[k]; !ok && !skip(k) {
				missing = append(missing, k)
			}
		}
		for k := range g {
			if _, ok := w[k]; !ok && !skip(k) {
				extra = append(extra, k)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 || len(extra) > 0 {
			return fmt.Errorf("%s: missing keys %v, unexpected keys %v", path, missing, extra)
		}
		for k := range w {
			if _, ok := g[k]; !ok {
				continue // missing but ignored
			}
			if err := sameShapeIgnoring(path+"."+k, w[k], g[k], ignore); err != nil {
				return err
			}
		}
	case []any:
		g := got.([]any)
		if len(w) > 0 && len(g) > 0 {
			return sameShapeIgnoring(path+"[0]", w[0], g[0], ignore)
		}
	}
	return nil
}

// newIntegrationServer is newTestServer backed by a throwaway Postgres
// database and Redis prefix, with the scrubbed captures' admin ("user1",
// password "REDACTED") bootstrapped so recorded logins replay as-is.
func newIntegrationServer(t *testing.T) (http.Handler, Deps) {
	t.Helper()
	q, pool := testutil.Queries(t)
	testPool = pool
	c := testutil.Cache(t)
	svc := auth.New(q, c, testutil.Discard())
	if err := svc.Bootstrap(t.Context(), "user1", "REDACTED"); err != nil {
		t.Fatal(err)
	}
	serverID, err := pg.EnsureServerID(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: config.Defaults(), ServerID: dto.IDFromUUID(serverID), Auth: svc, Queries: q, DB: pool, Log: testutil.Discard(), Cache: c}
	rt := jfapi.NewRouter(testutil.Discard(), jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	return rt, d
}

// testPool is the current integration test's database, for raw test SQL.
var testPool *pgxpool.Pool

// execSQL runs raw SQL against the integration test database.
func execSQL(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// captureToken is the access token every scrubbed capture carries
// (scrub_fixtures.py numbers tokens per capture, so each starts at 1).
const captureToken = "aaaa0000000000000000000000000001"

// seedCaptureToken makes captureToken valid for the bootstrapped "user1", so
// recorded requests replay unchanged.
func seedCaptureToken(t *testing.T, d Deps) {
	t.Helper()
	u, err := d.Queries.GetUserByName(t.Context(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Queries.UpsertDevice(t.Context(), db.UpsertDeviceParams{ID: "capture-device", UserID: u.ID, Name: "captures", AppName: "replay", AppVersion: "1"}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(captureToken))
	if err := d.Queries.CreateAccessToken(t.Context(), db.CreateAccessTokenParams{TokenSha: sum[:], UserID: u.ID, DeviceID: "capture-device"}); err != nil {
		t.Fatal(err)
	}
}

// checkContract replays every capture of endpoint against a server without a
// database and requires the same status and response shape. It returns the
// decoded responses for value checks.
func checkContract(t *testing.T, endpoint string) []any {
	t.Helper()
	h, _ := newTestServer(t)
	return checkContractOn(t, h, capturesFor(t, endpoint))
}

// checkContractOn is checkContract for a given handler and set of captures.
func checkContractOn(t *testing.T, h http.Handler, caps []capture, ignore ...string) []any {
	t.Helper()
	skip := map[string]bool{}
	for _, p := range ignore {
		skip[p] = true
	}
	var bodies []any
	for _, c := range caps {
		if c.Response.Status == 0 {
			continue // the proxy recorded the request but no response (client hung up)
		}
		rec := replay(t, h, c)
		if rec.Code != c.Response.Status {
			t.Errorf("%s: status %d, Jellyfin %d", c.Name, rec.Code, c.Response.Status)
			continue
		}
		if empty := len(c.Response.Body) == 0 || string(c.Response.Body) == "null"; empty || rec.Body.Len() == 0 {
			if !empty || rec.Body.Len() != 0 {
				t.Errorf("%s: body %q, Jellyfin %q", c.Name, rec.Body, c.Response.Body)
			}
			continue // no body on either side
		}
		var want, got any
		if err := json.Unmarshal(c.Response.Body, &want); err != nil {
			t.Fatalf("%s: captured body: %v", c.Name, err)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Errorf("%s: response is not JSON: %v (%q)", c.Name, err, rec.Body.String())
			continue
		}
		if err := sameShapeIgnoring("$", want, got, skip); err != nil {
			t.Errorf("%s: %v", c.Name, err)
		}
		bodies = append(bodies, got)
	}
	return bodies
}
