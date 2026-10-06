// Package images fetches, resizes and caches artwork (TASKS P1.17, DESIGN
// §2): originals come from a remote URL (TMDB) or a local file, are cached
// once, and every requested size is rendered once and kept on disk.
package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/buckket/go-blurhash"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decode WebP originals (encoding stays JPEG/PNG)
	"golang.org/x/sync/singleflight"
)

const (
	// MaxSourceBytes caps downloaded originals.
	MaxSourceBytes = 30 << 20
	// DefaultQuality matches Jellyfin's JPEG default.
	DefaultQuality = 90
	// sizeStep rounds requested sizes up, so clients asking for 389, 391 and
	// 400 px share one rendition instead of filling the cache with near-duplicates.
	sizeStep = 20
)

// ErrNoImage means the source is missing or not an image.
var ErrNoImage = errors.New("images: no usable image")

// Source is one stored image (images row or a person's photo).
type Source struct {
	URL       string // remote original
	LocalPath string // or a local file
	Tag       string // cache key; changes when the image changes
}

// Request is what a client asked for. Zero fields are unset.
type Request struct {
	Width, Height         int // exact box (keeps aspect ratio, fits inside)
	MaxWidth, MaxHeight   int
	FillWidth, FillHeight int // box to cover
	Quality               int
	Format                string // "jpg", "png", or "" for the original's format
}

// Rendition is a file ready to serve.
type Rendition struct {
	Path        string
	ContentType string
	ModTime     time.Time
}

// Original describes a cached original.
type Original struct {
	Width, Height int
	Blurhash      string
}

// Store caches originals and renditions under dir.
type Store struct {
	dir  string
	http *http.Client
	sf   singleflight.Group
}

// New returns a Store rooted at dir (paths.images).
func New(dir string) *Store {
	return &Store{dir: dir, http: &http.Client{Timeout: 30 * time.Second}}
}

// Tag is the cache tag for an image URL (also used for ImageTags).
func Tag(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:8])
}

// Get returns a rendition of src for req, building and caching it if needed.
func (s *Store) Get(ctx context.Context, src Source, req Request) (Rendition, error) {
	orig, err := s.original(ctx, src)
	if err != nil {
		return Rendition{}, err
	}
	cfg, format, err := decodeConfig(orig)
	if err != nil {
		return Rendition{}, err
	}
	outFormat := format
	switch strings.ToLower(req.Format) {
	case "jpg", "jpeg":
		outFormat = "jpeg"
	case "png":
		outFormat = "png"
	}
	if outFormat != "png" {
		outFormat = "jpeg" // WebP/GIF originals are served as JPEG
	}
	w, h := targetSize(cfg.Width, cfg.Height, req)
	q := req.Quality
	if q <= 0 || q > 100 {
		q = DefaultQuality
	}
	// The original itself, when nothing changes.
	if w == cfg.Width && h == cfg.Height && outFormat == format && (req.Quality == 0 || format == "png") {
		return rendition(orig, format)
	}
	name := fmt.Sprintf("%dx%d", w, h)
	if outFormat == "jpeg" {
		name += fmt.Sprintf("-q%d", q)
	}
	path := filepath.Join(s.dir, "r", shard(src.Tag), src.Tag, name+ext(outFormat))
	if _, err := os.Stat(path); err == nil {
		return rendition(path, outFormat)
	}
	_, err, _ = s.sf.Do(path, func() (any, error) {
		if _, err := os.Stat(path); err == nil {
			return nil, nil
		}
		img, err := decodeFile(orig)
		if err != nil {
			return nil, err
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		var buf bytes.Buffer
		if outFormat == "png" {
			err = png.Encode(&buf, dst)
		} else {
			err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: q})
		}
		if err != nil {
			return nil, err
		}
		return nil, writeAtomic(path, buf.Bytes())
	})
	if err != nil {
		return Rendition{}, err
	}
	return rendition(path, outFormat)
}

// Inspect returns the original's dimensions and blurhash (fetching it if needed).
func (s *Store) Inspect(ctx context.Context, src Source) (Original, error) {
	orig, err := s.original(ctx, src)
	if err != nil {
		return Original{}, err
	}
	img, err := decodeFile(orig)
	if err != nil {
		return Original{}, err
	}
	b := img.Bounds()
	// Blurhash only needs a tiny version; encoding the full image is slow.
	tw, th := targetSize(b.Dx(), b.Dy(), Request{MaxWidth: 32, MaxHeight: 32})
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.ApproxBiLinear.Scale(small, small.Bounds(), img, b, draw.Src, nil)
	x, y := 4, 3 // Jellyfin's component counts (landscape)
	if b.Dy() > b.Dx() {
		x, y = 3, 4
	}
	hash, err := blurhash.Encode(x, y, small)
	if err != nil {
		return Original{}, err
	}
	return Original{Width: b.Dx(), Height: b.Dy(), Blurhash: hash}, nil
}

// original returns the cached original's path, fetching it on first use.
func (s *Store) original(ctx context.Context, src Source) (string, error) {
	if src.LocalPath != "" {
		if _, err := os.Stat(src.LocalPath); err != nil {
			return "", fmt.Errorf("%w: %w", ErrNoImage, err)
		}
		return src.LocalPath, nil
	}
	if src.URL == "" || src.Tag == "" {
		return "", ErrNoImage
	}
	path := filepath.Join(s.dir, "orig", shard(src.Tag), src.Tag)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	_, err, _ := s.sf.Do(path, func() (any, error) {
		if _, err := os.Stat(path); err == nil {
			return nil, nil
		}
		data, err := s.download(ctx, src.URL)
		if err != nil {
			return nil, err
		}
		if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoImage, err)
		}
		return nil, writeAtomic(path, data)
	})
	return path, err
}

func (s *Store) download(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return nil, fmt.Errorf("%w: unsupported URL scheme", ErrNoImage)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("images: fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrNoImage, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSourceBytes {
		return nil, fmt.Errorf("%w: larger than %d bytes", ErrNoImage, MaxSourceBytes)
	}
	return data, nil
}

// targetSize applies Jellyfin's sizing parameters without ever upscaling.
// Width/Height and Max* fit the image inside the box; Fill* cover it (the
// client crops), which keeps detail for cropped thumbnails. Results are
// rounded up to sizeStep (capped at the original).
func targetSize(w, h int, r Request) (int, int) {
	if w <= 0 || h <= 0 {
		return w, h
	}
	scale := 1.0
	fit := func(bw, bh int) {
		s := 1.0
		if bw > 0 {
			s = min(s, float64(bw)/float64(w))
		}
		if bh > 0 {
			s = min(s, float64(bh)/float64(h))
		}
		scale = min(scale, s)
	}
	fit(r.Width, r.Height)
	fit(r.MaxWidth, r.MaxHeight)
	if r.FillWidth > 0 || r.FillHeight > 0 {
		s := 0.0
		if r.FillWidth > 0 {
			s = max(s, float64(r.FillWidth)/float64(w))
		}
		if r.FillHeight > 0 {
			s = max(s, float64(r.FillHeight)/float64(h))
		}
		scale = min(scale, s)
	}
	if scale >= 1 {
		return w, h
	}
	nw := roundUp(int(float64(w)*scale+0.5), w)
	nh := max(1, int(float64(h)*float64(nw)/float64(w)+0.5))
	return nw, min(nh, h)
}

func roundUp(n, limit int) int {
	n = (n + sizeStep - 1) / sizeStep * sizeStep
	return max(1, min(n, limit))
}

func decodeConfig(path string) (image.Config, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return image.Config{}, "", err
	}
	defer func() { _ = f.Close() }()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return image.Config{}, "", fmt.Errorf("%w: %w", ErrNoImage, err)
	}
	return cfg, format, nil
}

func decodeFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoImage, err)
	}
	return img, nil
}

func rendition(path, format string) (Rendition, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Rendition{}, err
	}
	ct := "image/jpeg"
	switch format {
	case "png":
		ct = "image/png"
	case "webp":
		ct = "image/webp"
	case "gif":
		ct = "image/gif"
	}
	return Rendition{Path: path, ContentType: ct, ModTime: fi.ModTime()}, nil
}

func ext(format string) string {
	if format == "png" {
		return ".png"
	}
	return ".jpg"
}

func shard(tag string) string {
	if len(tag) < 2 {
		return "00"
	}
	return tag[:2]
}

// writeAtomic writes data via a temp file + rename so readers never see a
// partial image.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
