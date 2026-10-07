package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// User management (TASKS P4.1), as Jellyfin's API does it: an admin creates,
// deletes and sets the policy of users; anyone changes their own password
// and name. Policy changes apply at once (cached sessions are dropped);
// disabling or deleting a user signs them out everywhere. The server never
// ends up without an enabled administrator.

func (a *api) registerUserAdmin(rt *jfapi.Router) {
	rt.Post("/Users/New", a.requireAdmin(a.createUser))
	rt.Delete("/Users/{userId}", a.requireAdmin(a.deleteUser))
	rt.Post("/Users/{userId}/Policy", a.requireAdmin(a.updatePolicy))
	rt.Post("/Users/{userId}/Password", a.requireUser(a.updatePassword))
	rt.Post("/Users/{userId}", a.requireUser(a.updateUser))
}

// targetUser reads the {userId} the request is about; a non-admin may only
// name themselves.
func (a *api) targetUser(w http.ResponseWriter, r *http.Request, s auth.Session) (db.User, bool) {
	id, err := dto.ParseID(jfapi.URLParam(r, "userId"))
	if err != nil {
		errorText(w, http.StatusBadRequest)
		return db.User{}, false
	}
	if id.UUID() != s.UserID && !s.IsAdmin {
		errorText(w, http.StatusForbidden)
		return db.User{}, false
	}
	u, err := a.Queries.GetUserByID(r.Context(), id.UUID())
	if errors.Is(err, pgx.ErrNoRows) {
		errorText(w, http.StatusNotFound)
		return u, false
	}
	if err != nil {
		a.internalError(w, r, err)
		return u, false
	}
	return u, true
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (a *api) createUser(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var body dto.CreateUserByName
	if err := jfapi.DecodeJSON(w, r, &body); err != nil || strings.TrimSpace(body.Name) == "" {
		errorText(w, http.StatusBadRequest)
		return
	}
	var hash *string
	if pw := deref(body.Password); pw != "" {
		h, err := auth.HashPassword(pw)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		hash = &h
	}
	u, err := a.Queries.CreateUser(r.Context(), db.CreateUserParams{Name: strings.TrimSpace(body.Name), PasswordHash: hash})
	if isUniqueViolation(err) {
		errorText(w, http.StatusBadRequest) // the name is taken
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "user created", "user", u.Name)
	d, err := a.userDto(u)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, d)
}

// keepsAnAdmin reports whether the server still has an enabled admin once
// u stops being one.
func (a *api) keepsAnAdmin(r *http.Request, u db.User) (bool, error) {
	if !u.IsAdmin || u.IsDisabled {
		return true, nil
	}
	n, err := a.Queries.CountEnabledAdmins(r.Context())
	return n > 1, err
}

// refuseLastAdmin answers 400 when the change would leave no enabled admin.
func (a *api) refuseLastAdmin(w http.ResponseWriter, r *http.Request, u db.User) bool {
	keeps, err := a.keepsAnAdmin(r, u)
	switch {
	case err != nil:
		a.internalError(w, r, err)
		return true
	case !keeps:
		errorText(w, http.StatusBadRequest)
		return true
	}
	return false
}

func (a *api) deleteUser(w http.ResponseWriter, r *http.Request, s auth.Session) {
	u, ok := a.targetUser(w, r, s)
	if !ok {
		return
	}
	if u.ID == s.UserID {
		errorText(w, http.StatusBadRequest) // not yourself
		return
	}
	if a.refuseLastAdmin(w, r, u) {
		return
	}
	if err := a.Auth.RevokeUser(r.Context(), u.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	if _, err := a.Queries.DeleteUser(r.Context(), u.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "user deleted", "user", u.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) updatePolicy(w http.ResponseWriter, r *http.Request, s auth.Session) {
	u, ok := a.targetUser(w, r, s)
	if !ok {
		return
	}
	var pol dto.UserPolicy
	if err := jfapi.DecodeJSON(w, r, &pol); err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	isAdmin, disabled := u.IsAdmin, u.IsDisabled
	if pol.IsAdministrator != nil {
		isAdmin = *pol.IsAdministrator
	}
	if pol.IsDisabled != nil {
		disabled = *pol.IsDisabled
	}
	if u.ID == s.UserID && (!isAdmin || disabled) {
		errorText(w, http.StatusBadRequest) // an admin can't demote or disable themselves
		return
	}
	if (!isAdmin || disabled) && a.refuseLastAdmin(w, r, u) {
		return
	}
	// The database owns the two flags; the rest is stored as sent (only the
	// keys the body set, in Jellyfin's casing).
	pol.IsAdministrator, pol.IsDisabled = nil, nil
	stored, err := json.Marshal(pol)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if _, err := a.Queries.UpdateUserPolicy(r.Context(), db.UpdateUserPolicyParams{ID: u.ID, IsAdmin: isAdmin, IsDisabled: disabled, Policy: stored}); err != nil {
		a.internalError(w, r, err)
		return
	}
	if disabled {
		err = a.Auth.RevokeUser(r.Context(), u.ID)
	} else {
		err = a.Auth.InvalidateUser(r.Context(), u.ID)
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "user policy updated", "user", u.Name, "admin", isAdmin, "disabled", disabled)
	w.WriteHeader(http.StatusNoContent)
}

// updatePassword: users change their own (CurrentPw must match, unless they
// have none); an admin sets anyone else's, or clears it (ResetPassword, as
// in Jellyfin).
func (a *api) updatePassword(w http.ResponseWriter, r *http.Request, s auth.Session) {
	u, ok := a.targetUser(w, r, s)
	if !ok {
		return
	}
	var body dto.UpdateUserPassword
	if err := jfapi.DecodeJSON(w, r, &body); err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	adminForOther := s.IsAdmin && u.ID != s.UserID
	if !adminForOther && u.PasswordHash != nil && !auth.CheckPassword(u.PasswordHash, deref(body.CurrentPw)) {
		errorText(w, http.StatusForbidden)
		return
	}
	var hash *string
	switch {
	case body.ResetPassword != nil && *body.ResetPassword:
		if !adminForOther {
			errorText(w, http.StatusForbidden)
			return
		}
	case deref(body.NewPw) == "":
		errorText(w, http.StatusBadRequest)
		return
	default:
		h, err := auth.HashPassword(*body.NewPw)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		hash = &h
	}
	if err := a.Queries.SetUserPassword(r.Context(), db.SetUserPasswordParams{ID: u.ID, PasswordHash: hash}); err != nil {
		a.internalError(w, r, err)
		return
	}
	a.Log.InfoContext(r.Context(), "user password changed", "user", u.Name, "by_admin", adminForOther)
	w.WriteHeader(http.StatusNoContent)
}

// updateUser renames a user: the one UserDto field set here (the policy and
// password have their own routes).
func (a *api) updateUser(w http.ResponseWriter, r *http.Request, s auth.Session) {
	u, ok := a.targetUser(w, r, s)
	if !ok {
		return
	}
	var body dto.UserDto
	if err := jfapi.DecodeJSON(w, r, &body); err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(deref(body.Name))
	if name == "" || name == u.Name {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if _, err := a.Queries.RenameUser(r.Context(), db.RenameUserParams{ID: u.ID, Name: name}); err != nil {
		if isUniqueViolation(err) {
			errorText(w, http.StatusBadRequest)
			return
		}
		a.internalError(w, r, err)
		return
	}
	if err := a.Auth.InvalidateUser(r.Context(), u.ID); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
