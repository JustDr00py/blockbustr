package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// quotaMonthStart is when the current calendar month began in the
// configured time zone (server.timezone), the one the Stats page's monthly
// totals use: monthly data quotas renew then.
func (a *api) quotaMonthStart(now time.Time) time.Time {
	loc, err := time.LoadLocation(a.live().Server.Timezone)
	if err != nil {
		loc = time.UTC
	}
	y, m, _ := now.In(loc).Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, loc)
}

// overQuota says whether s's user has used up their monthly data quota
// (policy MonthlyDataGB). Bytes of a stream still going count as they're
// flushed, so a play already started isn't cut off: the next one is
// refused.
func (a *api) overQuota(ctx context.Context, s auth.Session) (bool, error) {
	if s.MonthlyQuota <= 0 {
		return false, nil
	}
	used, err := a.Queries.UserBytesSince(ctx, db.UserBytesSinceParams{UserID: s.UserID, Since: a.quotaMonthStart(time.Now())})
	if err != nil {
		return false, err
	}
	return used >= s.MonthlyQuota, nil
}

func (a *api) registerQuota(rt *jfapi.Router) {
	rt.Get("/blockbustr/usage", a.requireAdmin(a.getUsage))
}

type userUsage struct {
	UserID uuid.UUID
	Bytes  int64
}

type usageResponse struct {
	Month string // "2026-10", in server.timezone
	Users []userUsage
}

// getUsage is every user's bytes this quota month, for the Users page.
func (a *api) getUsage(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	start := a.quotaMonthStart(time.Now())
	rows, err := a.Queries.UserBytesByUserSince(r.Context(), start)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := usageResponse{Month: start.Format("2006-01"), Users: make([]userUsage, 0, len(rows))}
	for _, u := range rows {
		out.Users = append(out.Users, userUsage(u))
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}
