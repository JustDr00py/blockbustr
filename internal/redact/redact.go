// Package redact keeps addon and debrid URLs out of logs and errors: they
// carry API keys, in the query (TorBox's token=) or the path (an addon's
// configuration, TorBox's Stremio key).
package redact

import (
	"context"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// urlRE finds URLs in free text, leaving out the punctuation that ends a
// sentence or precedes ffmpeg's ": Invalid data…".
var urlRE = regexp.MustCompile(`https?://[^\s"'<>]*[^\s"'<>:,.;)]`)

// URLs replaces each URL in s with its scheme and host ("https://host/…"),
// enough to tell which service failed.
func URLs(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlRE.ReplaceAllStringFunc(s, URL)
}

// URL is u reduced to its scheme and host.
func URL(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "<url>"
	}
	out := p.Scheme + "://" + p.Host
	if (p.Path != "" && p.Path != "/") || p.RawQuery != "" || p.Fragment != "" {
		out += "/…"
	}
	return out
}

// Error is err with the URLs in its message redacted; errors.Is and As
// still see err. Nil stays nil.
func Error(err error) error {
	if err == nil {
		return nil
	}
	if msg := err.Error(); URLs(msg) != msg {
		return redacted{err}
	}
	return err
}

type redacted struct{ err error }

func (e redacted) Error() string { return URLs(e.err.Error()) }
func (e redacted) Unwrap() error { return e.err }

// Handler redacts the URLs in each record's message and attribute values
// before inner sees it: a net under the redaction done where errors are
// made.
func Handler(inner slog.Handler) slog.Handler { return handler{inner} }

type handler struct{ inner slog.Handler }

func (h handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, URLs(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(attr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h handler) WithAttrs(as []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(as))
	for i, a := range as {
		clean[i] = attr(a)
	}
	return handler{h.inner.WithAttrs(clean)}
}

func (h handler) WithGroup(name string) slog.Handler { return handler{h.inner.WithGroup(name)} }

func attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		if s := v.String(); strings.Contains(s, "://") {
			return slog.String(a.Key, URLs(s))
		}
	case slog.KindGroup:
		g := v.Group()
		clean := make([]slog.Attr, len(g))
		for i, ga := range g {
			clean[i] = attr(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.Any(a.Key, Error(err))
		}
	}
	return a
}
