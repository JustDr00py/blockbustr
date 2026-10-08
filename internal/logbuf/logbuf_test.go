package logbuf

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestHandlerCapturesAtItsOwnLevel(t *testing.T) {
	var out bytes.Buffer
	b := New(10, slog.LevelInfo)
	log := slog.New(b.Handler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))

	log.Debug("hidden")
	b.SetLevel(slog.LevelDebug)
	log.Debug("request", "path", "/Items/x/PlaybackInfo", "status", 400)
	log.With("provider", "torbox").WithGroup("http").Warn("check failed", "err", errors.New("403"))

	got := b.Since(0, 0)
	if len(got) != 2 {
		t.Fatalf("captured %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Message != "request" || got[0].Level != "DEBUG" || got[0].Attrs[1] != (Attr{"status", "400"}) {
		t.Errorf("debug entry = %+v", got[0])
	}
	want := []Attr{{"provider", "torbox"}, {"http.err", "403"}}
	if got[1].Level != "WARN" || len(got[1].Attrs) != 2 || got[1].Attrs[0] != want[0] || got[1].Attrs[1] != want[1] {
		t.Errorf("warn entry = %+v, want attrs %v", got[1], want)
	}
	// stdout stays at its configured level: no debug lines there.
	if s := out.String(); strings.Contains(s, "DEBUG") || !strings.Contains(s, "check failed") {
		t.Errorf("stdout = %q", s)
	}
}

func TestSinceWrapsAndLimits(t *testing.T) {
	b := New(3, slog.LevelInfo)
	log := slog.New(b.Handler(slog.DiscardHandler))
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		log.Info(m)
	}
	msgs := func(es []Entry) string {
		var s []string
		for _, e := range es {
			s = append(s, e.Message)
		}
		return strings.Join(s, "")
	}
	if got := msgs(b.Since(0, 0)); got != "cde" {
		t.Errorf("Since(0) = %q, want cde (ring of 3)", got)
	}
	if got := msgs(b.Since(4, 0)); got != "e" {
		t.Errorf("Since(4) = %q, want e", got)
	}
	if got := msgs(b.Since(0, 2)); got != "de" {
		t.Errorf("Since(0, 2) = %q, want de", got)
	}
	if got := b.Since(5, 0); len(got) != 0 {
		t.Errorf("Since(5) = %v, want none", got)
	}
}
