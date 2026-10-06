// Package stremio is a client for the Stremio addon protocol (DESIGN §7.1):
// manifest, catalog (with extras), meta, stream and subtitles.
//
// An addon is addressed by its base URL: the manifest URL without
// "/manifest.json". Configured addons (Torrentio, Comet…) keep their
// settings, often including a debrid key, in that URL, so the base URL is a
// secret: errors and logs name only its host, and cache keys use a hash of
// it.
package stremio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sysadmin/blockbustr/internal/cache"
)

// DefaultTimeout bounds one addon request when the caller's context has no
// earlier deadline (DESIGN §7.3: 6s per addon when collecting streams).
const DefaultTimeout = 6 * time.Second

// maxBody bounds a response; a long series meta is ~1 MB.
const maxBody = 16 << 20

// Client talks to Stremio addons. Safe for concurrent use.
type Client struct {
	// HTTP makes the requests; nil means a client with no timeout of its
	// own (Timeout and the context bound each request).
	HTTP *http.Client
	// Timeout bounds each request; 0 means DefaultTimeout.
	Timeout time.Duration
	// Cache keeps streams (30 min) and metas (24 h); nil disables caching.
	Cache *cache.Cache
	// UserAgent is sent with every request; "" means "blockbustr".
	UserAgent string
}

// ErrBadURL reports an addon URL that isn't http(s) or stremio://.
var ErrBadURL = errors.New("stremio: addon URL must be http(s):// or stremio://")

// BaseURL normalises an addon URL as users paste it (".../manifest.json",
// "stremio://host/...") into its base URL.
func BaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(s, "stremio://"); ok {
		s = "https://" + rest
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrBadURL
	}
	u.Fragment = ""
	u.RawQuery = ""
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/manifest.json"), "/")
	u.RawPath = strings.TrimSuffix(strings.TrimSuffix(u.RawPath, "/manifest.json"), "/")
	return u.String(), nil
}

// Redact names an addon without its secret parts: scheme and host only.
func Redact(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "addon"
	}
	return u.Scheme + "://" + u.Host
}

// AddonKey identifies an addon (base URL, config included) in cache keys
// without revealing it. A changed configuration is a different addon.
func AddonKey(base string) string {
	sum := sha256.Sum256([]byte(base))
	return hex.EncodeToString(sum[:8])
}

// Manifest fetches the addon's manifest.
func (c *Client) Manifest(ctx context.Context, base string) (Manifest, error) {
	var m Manifest
	err := c.get(ctx, base, "manifest.json", &m)
	if err == nil && m.ID == "" {
		err = fmt.Errorf("stremio %s: manifest has no id", Redact(base))
	}
	return m, err
}

// Catalog fetches one page of a catalog.
func (c *Client) Catalog(ctx context.Context, base, typ, id string, extra Extra) ([]Meta, error) {
	var out struct {
		Metas []Meta `json:"metas"`
	}
	path := "catalog/" + seg(typ) + "/" + seg(id)
	if e := extra.encode(); e != "" {
		path += "/" + e
	}
	err := c.get(ctx, base, path+".json", &out)
	return out.Metas, err
}

// encode renders extras like the Stremio SDK: name=value pairs, values
// escaped like encodeURIComponent (space is %20, not +), in a stable order.
func (e Extra) encode() string {
	var parts []string
	add := func(k, v string) {
		parts = append(parts, k+"="+strings.ReplaceAll(url.QueryEscape(v), "+", "%20"))
	}
	if e.Genre != "" {
		add("genre", e.Genre)
	}
	if e.Search != "" {
		add("search", e.Search)
	}
	if e.Skip > 0 {
		add("skip", strconv.Itoa(e.Skip))
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// Meta fetches a title's full meta (a series' episodes are in Videos).
func (c *Client) Meta(ctx context.Context, base, typ, id string) (Meta, error) {
	key := cache.StremioMetaKey(AddonKey(base), typ, id)
	var out struct {
		Meta *Meta `json:"meta"`
	}
	if c.cached(ctx, key, &out.Meta) {
		return *out.Meta, nil
	}
	if err := c.get(ctx, base, "meta/"+seg(typ)+"/"+seg(id)+".json", &out); err != nil {
		return Meta{}, err
	}
	if out.Meta == nil {
		return Meta{}, fmt.Errorf("stremio %s: no meta for %s %s", Redact(base), typ, id)
	}
	c.store(ctx, key, out.Meta, cache.StremioMetaTTL)
	return *out.Meta, nil
}

// Streams fetches the streams offered for a title. Series episodes use
// "{imdb}:{season}:{episode}" ids.
func (c *Client) Streams(ctx context.Context, base, typ, id string) ([]Stream, error) {
	key := cache.StremioStreamsKey(AddonKey(base), typ, id)
	var out struct {
		Streams []Stream `json:"streams"`
	}
	if c.cached(ctx, key, &out.Streams) {
		return out.Streams, nil
	}
	if err := c.get(ctx, base, "stream/"+seg(typ)+"/"+seg(id)+".json", &out); err != nil {
		return nil, err
	}
	for i := range out.Streams {
		out.Streams[i].InfoHash = strings.ToLower(out.Streams[i].InfoHash)
	}
	if out.Streams == nil {
		out.Streams = []Stream{} // cache "none" too
	}
	c.store(ctx, key, out.Streams, cache.StremioStreamsTTL)
	return out.Streams, nil
}

// Subtitles fetches the subtitles offered for a title.
func (c *Client) Subtitles(ctx context.Context, base, typ, id string) ([]Subtitle, error) {
	key := cache.StremioSubtitlesKey(AddonKey(base), typ, id)
	var out struct {
		Subtitles []Subtitle `json:"subtitles"`
	}
	if c.cached(ctx, key, &out.Subtitles) {
		return out.Subtitles, nil
	}
	if err := c.get(ctx, base, "subtitles/"+seg(typ)+"/"+seg(id)+".json", &out); err != nil {
		return nil, err
	}
	if out.Subtitles == nil {
		out.Subtitles = []Subtitle{}
	}
	c.store(ctx, key, out.Subtitles, cache.StremioSubsTTL)
	return out.Subtitles, nil
}

// seg escapes a path segment, keeping ':' (episode ids) readable.
func seg(s string) string { return url.PathEscape(s) }

func (c *Client) cached(ctx context.Context, key cache.Key, dst any) bool {
	if c.Cache == nil {
		return false
	}
	ok, err := c.Cache.GetJSON(ctx, key, dst)
	return err == nil && ok
}

func (c *Client) store(ctx context.Context, key cache.Key, v any, ttl time.Duration) {
	if c.Cache != nil {
		_ = c.Cache.SetJSON(ctx, key, v, ttl) // a cache failure only costs a refetch
	}
}

// get fetches base/path into out. Errors never contain base itself.
func (c *Client) get(ctx context.Context, base, path string, out any) error {
	name := Redact(base)
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("stremio %s: bad addon URL", name)
	}
	// Keep path exactly as encoded: Go would otherwise re-normalise it and
	// turn an escaped "/" or "&" in a search term into a separator.
	u.RawPath = strings.TrimSuffix(u.EscapedPath(), "/") + "/" + path
	if u.Path, err = url.PathUnescape(u.RawPath); err != nil {
		return fmt.Errorf("stremio %s: bad path", name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "", nil)
	if err != nil {
		return fmt.Errorf("stremio %s: bad request", name)
	}
	req.URL, req.Host = u, u.Host
	ua := c.UserAgent
	if ua == "" {
		ua = "blockbustr"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error // its message repeats the URL
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("stremio %s %s: %w", name, resource(path), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stremio %s %s: HTTP %d", name, resource(path), resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("stremio %s %s: decode: %w", name, resource(path), err)
	}
	return nil
}

// resource is the part of path safe to log: "stream/movie/tt0133093". Catalog
// extras may hold a search term, which is fine to log; the base is not.
func resource(path string) string { return strings.TrimSuffix(path, ".json") }
