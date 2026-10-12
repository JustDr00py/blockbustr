package redact

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

const secret = "SECRETKEY123"

func TestURLs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://store-021.weur.tb-cdn.st/zip/abc?token=" + secret + ": Invalid data found when processing input",
			"https://store-021.weur.tb-cdn.st/…: Invalid data found when processing input"},
		{"Get \"https://api.torbox.app/v1/api/torrents/requestdl?token=" + secret + "&file_id=3\": dial tcp: timeout",
			"Get \"https://api.torbox.app/…\": dial tcp: timeout"},
		{"open https://torrentio.strem.fun/realdebrid=" + secret + "/resolve/x.mkv, retrying",
			"open https://torrentio.strem.fun/…, retrying"},
		{"https://user:" + secret + "@host.example/a?apikey=" + secret + ".", "https://host.example/…."},
		{"listening on http://0.0.0.0:8097", "listening on http://0.0.0.0:8097"},
		{"no urls here", "no urls here"},
	}
	for _, c := range cases {
		if got := URLs(c.in); got != c.want {
			t.Errorf("URLs(%q)\n = %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestError(t *testing.T) {
	if Error(nil) != nil {
		t.Error("nil error not nil")
	}
	if errors.As(Error(errors.New("no url")), new(redacted)) {
		t.Error("an error without a URL was wrapped")
	}
	// As http.Client.Do fails.
	err := &url.Error{Op: "Get", URL: "http://127.0.0.1:1/requestdl?token=" + secret, Err: errors.New("connection refused")}
	red := Error(err)
	if strings.Contains(red.Error(), secret) {
		t.Errorf("token kept: %v", red)
	}
	if !strings.Contains(red.Error(), "127.0.0.1:1") {
		t.Errorf("host lost: %v", red)
	}
	wrapped := Error(errors.Join(fs.ErrNotExist, errors.New("https://x.example/?key="+secret)))
	if !errors.Is(wrapped, fs.ErrNotExist) || strings.Contains(wrapped.Error(), secret) {
		t.Errorf("errors.Is lost or key kept: %v", wrapped)
	}
}

func TestHandler(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(Handler(slog.NewTextHandler(&out, nil)))
	link := "https://store.tb-cdn.st/dl?token=" + secret
	log.With("base", link).WithGroup("g").Info("failed "+link,
		"err", errors.New("ffprobe: exit status 1: "+link+": Invalid data"),
		"url", link, slog.Group("inner", "u", link), "n", 3)
	if strings.Contains(out.String(), secret) {
		t.Errorf("key logged: %s", out.String())
	}
	if !strings.Contains(out.String(), "store.tb-cdn.st") || !strings.Contains(out.String(), "n=3") {
		t.Errorf("lost the host or other attrs: %s", out.String())
	}
	if !Handler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})).Enabled(context.Background(), slog.LevelWarn) {
		t.Error("level not passed through")
	}
}
