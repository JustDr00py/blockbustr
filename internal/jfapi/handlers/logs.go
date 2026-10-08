package handlers

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/logbuf"
)

// The admin UI's Logs page: GET /blockbustr/logs?after=<seq>&limit=<n>
// returns the recent records after seq (oldest first) and the capture
// level; POST /blockbustr/logs/level {"Level": "debug"} changes that level
// until the next restart. The container's own log keeps the configured
// level either way.

type logsResponse struct {
	Level   string
	Entries []logbuf.Entry
}

func (a *api) registerLogs(rt *jfapi.Router) {
	if a.Logs == nil {
		return
	}
	rt.Get("/blockbustr/logs", a.requireAdmin(a.getLogs))
	rt.Post("/blockbustr/logs/level", a.requireAdmin(a.setLogLevel))
}

func (a *api) getLogs(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := jfapi.QueryOf(r)
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	limit, ok := q.Int("limit")
	if !ok || limit <= 0 {
		limit = 1000
	}
	entries := a.Logs.Since(after, limit)
	if entries == nil {
		entries = []logbuf.Entry{}
	}
	jfapi.WriteJSON(w, r, http.StatusOK, logsResponse{Level: a.Logs.Level().String(), Entries: entries})
}

func (a *api) setLogLevel(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var body struct{ Level string }
	var level slog.Level
	if err := jfapi.DecodeJSON(w, r, &body); err != nil || level.UnmarshalText([]byte(strings.TrimSpace(body.Level))) != nil {
		jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "Level is one of debug, info, warn, error"})
		return
	}
	a.Logs.SetLevel(level)
	a.Log.InfoContext(r.Context(), "log capture level changed", "by", s.UserName, "level", level.String())
	noContent(w)
}
