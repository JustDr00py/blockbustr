// Package logbuf keeps the most recent log records in memory for the admin
// UI's Logs page. Its handler wraps the stdout one: records still go out at
// the configured level, while the buffer captures at its own level, which
// the page can lower to debug for a while without touching the
// configuration or flooding the container's log.
package logbuf

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Entry is one captured record, its attributes flattened to strings
// (a group's keys joined with dots, as slog's text handler does).
type Entry struct {
	Seq     uint64
	Time    time.Time
	Level   string
	Message string
	Attrs   []Attr `json:",omitempty"`
}

type Attr struct {
	Key   string
	Value string
}

// Buffer is a fixed-size ring of entries, safe for concurrent use.
type Buffer struct {
	level slog.LevelVar

	mu      sync.Mutex
	entries []Entry // ring; entries[next] is the oldest once full
	next    int
	full    bool
	seq     uint64
}

// New returns a buffer of size entries capturing at level and above.
func New(size int, level slog.Level) *Buffer {
	b := &Buffer{entries: make([]Entry, size)}
	b.level.Set(level)
	return b
}

func (b *Buffer) Level() slog.Level         { return b.level.Level() }
func (b *Buffer) SetLevel(level slog.Level) { b.level.Set(level) }

func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	b.entries[b.next] = e
	b.next = (b.next + 1) % len(b.entries)
	if b.next == 0 {
		b.full = true
	}
}

// Since returns the entries after seq after, oldest first, at most the
// newest limit of them (limit <= 0: all).
func (b *Buffer) Since(after uint64, limit int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var all []Entry
	if b.full {
		all = append(append(all, b.entries[b.next:]...), b.entries[:b.next]...)
	} else {
		all = append(all, b.entries[:b.next]...)
	}
	i := len(all)
	for i > 0 && all[i-1].Seq > after {
		i--
	}
	out := all[i:]
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Handler returns a handler sending records to inner at inner's level and
// capturing them into b at b's.
func (b *Buffer) Handler(inner slog.Handler) slog.Handler {
	return &handler{buf: b, inner: inner}
}

type handler struct {
	buf    *Buffer
	inner  slog.Handler
	attrs  []Attr // from WithAttrs, already prefixed
	prefix string // open groups, "a.b."
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.buf.Level() || h.inner.Enabled(ctx, l)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.inner.Enabled(ctx, r.Level) {
		err = h.inner.Handle(ctx, r)
	}
	if r.Level >= h.buf.Level() {
		e := Entry{Time: r.Time, Level: r.Level.String(), Message: r.Message}
		e.Attrs = append(e.Attrs, h.attrs...)
		r.Attrs(func(a slog.Attr) bool {
			e.Attrs = flatten(e.Attrs, h.prefix, a)
			return true
		})
		h.buf.add(e)
	}
	return err
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.inner = h.inner.WithAttrs(as)
	c.attrs = append([]Attr(nil), h.attrs...)
	for _, a := range as {
		c.attrs = flatten(c.attrs, h.prefix, a)
	}
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.inner = h.inner.WithGroup(name)
	c.prefix = h.prefix + name + "."
	return &c
}

func flatten(out []Attr, prefix string, a slog.Attr) []Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range v.Group() {
			out = flatten(out, p, g)
		}
		return out
	}
	if a.Key == "" {
		return out
	}
	return append(out, Attr{Key: prefix + a.Key, Value: strings.TrimSpace(v.String())})
}
