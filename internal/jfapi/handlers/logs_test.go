package handlers

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"testing"

	"github.com/sysadmin/blockbustr/internal/config"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/logbuf"
)

// The Logs page reads recent records, and lowering the capture level to
// debug lets request lines through.
func TestLogsEndpoints(t *testing.T) {
	_, d := newIntegrationServer(t)
	seedCaptureToken(t, d)
	buf := logbuf.New(100, slog.LevelInfo)
	log := slog.New(buf.Handler(slog.DiscardHandler))
	d.Logs, d.Log, d.Config = buf, log, config.Defaults()
	rt := jfapi.NewRouter(log, jfapi.Options{LegacyAuth: true})
	Register(rt, d)
	admin := `MediaBrowser Token="` + captureToken + `"`

	list := func(after uint64) logsResponse {
		t.Helper()
		rec := call(t, rt, "GET", "/blockbustr/logs?after="+strconv.FormatUint(after, 10), admin, "")
		var out logsResponse
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		return out
	}
	if got := list(0); got.Level != "INFO" {
		t.Errorf("level = %q, want INFO", got.Level)
	}
	for body, want := range map[string]int{`{"Level":"loud"}`: 400, `{"Level":"debug"}`: 204} {
		if rec := call(t, rt, "POST", "/blockbustr/logs/level", admin, body); rec.Code != want {
			t.Errorf("%s: %d, want %d", body, rec.Code, want)
		}
	}
	got := list(0)
	if got.Level != "DEBUG" || len(got.Entries) == 0 {
		t.Fatalf("after debug: %+v", got)
	}
	last := got.Entries[len(got.Entries)-1]
	if last.Message != "request" { // the previous GET's own debug request line
		t.Errorf("last entry = %+v, want a request line", last)
	}
	if more := list(last.Seq).Entries; len(more) != 0 && more[0].Seq <= last.Seq {
		t.Errorf("after=%d returned older entries: %+v", last.Seq, more)
	}
	if rec := call(t, rt, "GET", "/blockbustr/logs", "", ""); rec.Code != 401 {
		t.Errorf("anonymous: %d", rec.Code)
	}
}
