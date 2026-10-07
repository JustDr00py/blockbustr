// Package resolve turns a source into the URL its bytes come from (DESIGN
// §6 .strm, §7.3, §8.2):
//   - File: a local path, served as is.
//   - URL: a .strm target or a Stremio stream `url`, followed through its
//     redirects. A jellybird `/stream/{provider}/{torrent}/{file}` target is
//     resolved straight through the same debrid account when blockbustr has
//     it, skipping jellybird; otherwise (or if that fails) it is followed.
//   - Debrid: a file of a torrent on a debrid account, via FileLink.
//   - Torrent: a Stremio infoHash stream, found on or added to a debrid
//     account (torrent.go).
//
// Resolved links are cached in Redis (link:*) for the provider's link
// lifetime (cache.LinkTTL when it doesn't say), and concurrent resolves of
// one source share a single provider call.
package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/metrics"
	"github.com/sysadmin/blockbustr/internal/provider"
)

// Kind is what a Source points at.
type Kind int

// Source kinds.
const (
	File    Kind = iota + 1 // Path
	URL                     // URL
	Debrid                  // Provider, TorrentID, FileID
	Torrent                 // InfoHash, FileIdx
)

// String names k for metrics and logs.
func (k Kind) String() string {
	switch k {
	case File:
		return "file"
	case URL:
		return "url"
	case Debrid:
		return "debrid"
	case Torrent:
		return "torrent"
	}
	return "unknown"
}

// Source is something playable.
type Source struct {
	Kind Kind
	Path string // File
	URL  string // URL

	Provider          provider.Name // Debrid
	TorrentID, FileID string

	InfoHash string // Torrent
	FileIdx  int    // -1: unknown
	FileName string // the file's name, when the addon gave it
}

// FromURL is a .strm target or a Stremio stream url.
func FromURL(u string) Source {
	if src, ok := fromMagnet(u); ok {
		return src
	}
	return Source{Kind: URL, URL: u}
}

// Magnet is the target string a Torrent source is stored as (a Stremio
// infoHash stream chosen at PlaybackInfo): a magnet link, plus the file
// index and name when the addon gave them. FromURL reads it back.
func Magnet(infoHash string, fileIdx int, fileName string) string {
	m := "magnet:?xt=urn:btih:" + strings.ToLower(infoHash)
	if fileIdx >= 0 {
		m += "&bb.file=" + strconv.Itoa(fileIdx)
	}
	if fileName != "" {
		m += "&bb.name=" + url.QueryEscape(fileName)
	}
	return m
}

func fromMagnet(u string) (Source, bool) {
	rest, ok := strings.CutPrefix(u, "magnet:?")
	if !ok {
		return Source{}, false
	}
	q, err := url.ParseQuery(rest)
	if err != nil {
		return Source{}, false
	}
	hash, ok := strings.CutPrefix(q.Get("xt"), "urn:btih:")
	if !ok || hash == "" {
		return Source{}, false
	}
	src := Source{Kind: Torrent, InfoHash: strings.ToLower(hash), FileIdx: -1}
	if n, err := strconv.Atoi(q.Get("bb.file")); err == nil && n >= 0 {
		src.FileIdx = n
	}
	src.FileName = q.Get("bb.name")
	return src, true
}

// ErrNotResolvable is returned for sources this build can't play yet (an
// infoHash before P3.8) or whose provider isn't configured.
var ErrNotResolvable = errors.New("resolve: source can't be resolved")

// key is the source's link:* cache key; "" for sources that aren't cached.
func (s Source) key() string {
	var id string
	switch s.Kind {
	case URL:
		id = s.URL // unchanged from P2.4b, so cached .strm links survive the upgrade
	case Debrid:
		id = "debrid\x00" + string(s.Provider) + "\x00" + s.TorrentID + "\x00" + s.FileID
	case Torrent:
		id = "torrent\x00" + s.InfoHash + "\x00" + strconv.Itoa(s.FileIdx) + "\x00" + s.FileName
	default:
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// jellybird maps a jellybird stream URL (…/stream/{provider}/{torrent}/{file},
// any base path, any query) to the debrid file it stands for.
func jellybird(target string) (Source, bool) {
	u, err := url.Parse(target)
	if err != nil {
		return Source{}, false
	}
	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(seg) < 4 || seg[len(seg)-4] != "stream" {
		return Source{}, false
	}
	seg = seg[len(seg)-3:]
	name, err := provider.ParseName(seg[0])
	if err != nil || seg[1] == "" || seg[2] == "" {
		return Source{}, false
	}
	return Source{Kind: Debrid, Provider: name, TorrentID: seg[1], FileID: seg[2]}, true
}

// ErrPlaceholder is a link that plays an addon's notice video instead of
// the title: a debrid-configured addon (Torrentio, AIOStreams) never fails
// with an error, it redirects to "failed_access.mp4" and the like.
var ErrPlaceholder = errors.New("resolve: the addon answered with a notice video")

// notice matches an addon's notice video by name: Torrentio's
// /videos/{failed_access,failed_infringement,downloading,…}_vN.mp4 and
// look-alikes under /static/.
var notice = regexp.MustCompile(`(?i)/(?:videos?|static)/(?:[^/]+/)*((?:failed|downloading|download|error|limit|limits|unavailable|not_?ready|no_)[a-z0-9_-]*)\.mp4$`)

// placeholder reports whether l is an addon's notice video. "downloading"
// ones mean the debrid service is fetching the torrent.
func placeholder(l Link) error {
	u, err := url.Parse(l.URL)
	if err != nil {
		return nil
	}
	m := notice.FindStringSubmatch(u.Path)
	if m == nil {
		return nil
	}
	if strings.HasPrefix(strings.ToLower(m[1]), "downloading") {
		return fmt.Errorf("%w (%s)", ErrDownloading, m[1])
	}
	return fmt.Errorf("%w (%s)", ErrPlaceholder, m[1])
}

// UserAgent is sent when following and fetching links. Some addon hosts
// (Torrentio's resolve URLs, behind Cloudflare) answer Go's default agent
// with 403.
const UserAgent = "blockbustr"

// Link is a resolved source.
type Link struct {
	URL  string // final URL after redirects; the path for a File
	Size int64  // bytes; 0 if unknown
	// Private links are bound to a debrid account: they must never reach a
	// client (AGENTS Safety), so they're proxied, not redirected to.
	Private bool
}

// debridHosts are the download hosts of debrid services. A followed URL
// (a .strm target, an addon's resolve URL) that ends on one is private too.
var debridHosts = []string{
	"real-debrid.com", "rdeb.io", "rdb.so", "torbox.app", "tb-cdn.st", "tb-cdn.io",
	"alldebrid.com", "alldebrid.fr", "debrid.it", "premiumize.me", "debrid-link.com", "debrid-link.fr",
	"offcloud.com", "easydebrid.com",
}

// IsPrivate reports whether l must stay on the server: resolved through a
// debrid account, or on a debrid download host. (Links cached before the
// flag existed are caught by their host.)
func IsPrivate(l Link) bool {
	if l.Private {
		return true
	}
	u, err := url.Parse(l.URL)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range debridHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// Resolver resolves and caches links.
type Resolver struct {
	// HTTP follows the redirects; nil means a client with a 15s timeout.
	HTTP *http.Client
	// Cache stores resolved links for cache.LinkTTL; nil disables caching.
	Cache *cache.Cache
	// Stream fetches link bytes for proxying; nil means a client that waits
	// at most 20s for response headers and never times out a body.
	Stream *http.Client
	// Providers are the configured debrid accounts; Order lists them by
	// priority, for adding torrents.
	Providers map[provider.Name]provider.Provider
	Order     []provider.Name
	// Accounts, when set, replaces Providers/Order: the live set an admin
	// can change without a restart.
	Accounts *provider.Set
	// Torrents remembers the torrents added to the accounts; nil adds a
	// torrent on every resolve that misses the link cache.
	Torrents TorrentStore
	// Log reports a jellybird target that had to be followed after all.
	Log *slog.Logger

	flight singleflight.Group
}

var defaultStream = &http.Client{Transport: &http.Transport{
	Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 20 * time.Second, IdleConnTimeout: 90 * time.Second,
}}

// Open requests a resolved link for proxying, passing the client's Range
// and If-Range on. method is GET or HEAD.
func (r *Resolver) Open(ctx context.Context, method, link string, from http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	for _, k := range []string{"Range", "If-Range"} {
		if v := from.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	c := r.Stream
	if c == nil {
		c = defaultStream
	}
	return c.Do(req)
}

func (r *Resolver) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Resolve returns where src's bytes are, from the cache when it's there.
func (r *Resolver) Resolve(ctx context.Context, src Source) (l Link, err error) {
	cached := false
	defer func() {
		result := "ok"
		switch {
		case errors.Is(err, ErrDownloading):
			result = "downloading"
		case err != nil:
			result = "error"
		case cached:
			result = "cached"
		}
		metrics.Resolves.WithLabelValues(src.Kind.String(), result).Inc()
	}()
	switch src.Kind {
	case File:
		fi, err := os.Stat(src.Path)
		if err != nil {
			return Link{}, fmt.Errorf("resolve: %w", err)
		}
		return Link{URL: src.Path, Size: fi.Size()}, nil
	case URL, Debrid, Torrent:
	default:
		return Link{}, fmt.Errorf("%w: kind %d", ErrNotResolvable, src.Kind)
	}
	key := src.key()
	if r.Cache != nil {
		if ok, err := r.Cache.GetJSON(ctx, cache.LinkKey(key), &l); err == nil && ok {
			cached = true
			return l, nil
		}
	}
	v, err, _ := r.flight.Do(key, func() (any, error) {
		l, ttl, err := r.resolve(ctx, src)
		if err == nil && r.Cache != nil {
			_ = r.Cache.SetJSON(ctx, cache.LinkKey(key), l, ttl) // a cache failure only costs a re-resolve
		}
		return l, err
	})
	if err != nil {
		return Link{}, err
	}
	return v.(Link), nil
}

func (r *Resolver) resolve(ctx context.Context, src Source) (Link, time.Duration, error) {
	switch src.Kind {
	case Debrid:
		return r.debrid(ctx, src)
	case Torrent:
		return r.torrent(ctx, src)
	}
	if d, ok := jellybird(src.URL); ok && r.account(d.Provider) != nil {
		l, ttl, err := r.debrid(ctx, d)
		if err == nil {
			return l, ttl, nil
		}
		// Not on this account (jellybird may use another), or the provider
		// is down: jellybird may still manage.
		if r.Log != nil {
			r.Log.WarnContext(ctx, "jellybird target not resolvable directly; following it", "provider", d.Provider, "err", err)
		}
	}
	l, err := r.follow(ctx, src.URL)
	if err == nil {
		err = placeholder(l)
	}
	l.Private = err == nil && IsPrivate(l)
	return l, cache.LinkTTL, err
}

// debrid asks the provider for a fresh link to a file, cached for the
// lifetime the provider gives (clamped to 5 min–24 h).
func (r *Resolver) debrid(ctx context.Context, src Source) (Link, time.Duration, error) {
	p := r.account(src.Provider)
	if p == nil {
		return Link{}, 0, fmt.Errorf("%w: no %s account", ErrNotResolvable, src.Provider)
	}
	link, expiry, err := p.FileLink(ctx, src.TorrentID, src.FileID)
	if err != nil {
		return Link{}, 0, fmt.Errorf("resolve: %s: %w", src.Provider, err)
	}
	return Link{URL: link, Private: true}, linkTTL(expiry, time.Now()), nil
}

func linkTTL(expiry, now time.Time) time.Duration {
	if expiry.IsZero() {
		return cache.LinkTTL
	}
	return min(max(expiry.Sub(now), 5*time.Minute), 24*time.Hour)
}

// follow follows target's redirects with a one-byte range GET (cheap, and
// accepted where HEAD often isn't) and returns the final URL and size.
func (r *Resolver) follow(ctx context.Context, target string) (Link, error) {
	var l Link
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return l, fmt.Errorf("resolve: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", UserAgent)
	resp, err := r.client().Do(req)
	if err != nil {
		return l, fmt.Errorf("resolve: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return l, fmt.Errorf("resolve: %s answered %s", resp.Request.URL.Host, resp.Status)
	}
	l.URL = resp.Request.URL.String()
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		l.Size, _ = strconv.ParseInt(cr[strings.LastIndex(cr, "/")+1:], 10, 64)
	} else if resp.StatusCode == http.StatusOK {
		l.Size = max(resp.ContentLength, 0)
	}
	return l, nil
}

// account is the debrid account for name, or nil.
func (r *Resolver) account(name provider.Name) provider.Provider {
	if r.Accounts != nil {
		return r.Accounts.Get(name)
	}
	return r.Providers[name]
}

// Forget drops src's cached link (it expired or stopped working).
func (r *Resolver) Forget(ctx context.Context, src Source) {
	if key := src.key(); key != "" && r.Cache != nil {
		_ = r.Cache.Delete(ctx, cache.LinkKey(key))
	}
}
