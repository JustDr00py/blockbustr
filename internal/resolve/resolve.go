// Package resolve turns a remote source into the URL its bytes come from
// (TASKS P2.4b, DESIGN §6, §8.2). Today that is a .strm target followed
// through its redirects (jellybird → debrid CDN); debrid and Stremio
// sources join in P3.2. Resolved links are cached in Redis (link:*), so a
// play doesn't repeat the redirect chain, which may unrestrict a debrid
// link each time.
package resolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sysadmin/blockbustr/internal/cache"
)

// Link is a resolved source.
type Link struct {
	URL  string // final URL after redirects
	Size int64  // bytes; 0 if the server didn't say
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

func linkKey(target string) cache.Key {
	sum := sha256.Sum256([]byte(target))
	return cache.LinkKey(hex.EncodeToString(sum[:]))
}

func (r *Resolver) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Resolve returns target's final URL, from the cache when it's there.
func (r *Resolver) Resolve(ctx context.Context, target string) (Link, error) {
	var l Link
	if r.Cache != nil {
		if ok, err := r.Cache.GetJSON(ctx, linkKey(target), &l); err == nil && ok {
			return l, nil
		}
	}
	// A one-byte range request: cheap, and accepted where HEAD often isn't.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return l, fmt.Errorf("resolve: %w", err)
	}
	req.Header.Set("Range", "bytes=0-0")
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
	if r.Cache != nil {
		_ = r.Cache.SetJSON(ctx, linkKey(target), l, cache.LinkTTL) // a cache failure only costs a re-resolve
	}
	return l, nil
}

// Forget drops target's cached link (it expired or stopped working).
func (r *Resolver) Forget(ctx context.Context, target string) {
	if r.Cache != nil {
		_ = r.Cache.Delete(ctx, linkKey(target))
	}
}
