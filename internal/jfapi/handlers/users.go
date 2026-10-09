package handlers

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Jellyfin 12.1.0's defaults for a user's Configuration and Policy, copied
// from a real /Users/Me response. Per-user overrides live in users.configuration
// and users.policy (jsonb) and are applied on top.
var (
	//go:embed defaults/user_configuration.json
	defaultUserConfiguration []byte
	//go:embed defaults/user_policy.json
	defaultUserPolicy []byte
)

func (a *api) registerUsers(rt *jfapi.Router) {
	rt.Post("/Users/AuthenticateByName", a.authenticateByName)
	rt.Get("/Users/Me", a.requireUser(a.usersMe))
	// The default policy has IsHidden=true, so the public list (shown on the
	// login screen) is empty, exactly as 12.1.0 returns it.
	rt.Get("/Users/Public", func(w http.ResponseWriter, r *http.Request) {
		jfapi.WriteJSON(w, r, http.StatusOK, []dto.UserDto{})
	})
	rt.Get("/Users", a.requireAdmin(a.listUsers))
	rt.Get("/Users/{userId}", a.requireUser(a.userByID))
	rt.Post("/Sessions/Logout", a.requireUser(a.logout))
}

func (a *api) authenticateByName(w http.ResponseWriter, r *http.Request) {
	info := jfapi.AuthFrom(r.Context())
	if !info.HasClient() {
		// 12.1.0 answers 400 when the Authorization header lacks client/device.
		errorText(w, http.StatusBadRequest)
		return
	}
	var body dto.AuthenticateUserByName
	if err := jfapi.DecodeJSON(w, r, &body); err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	u, err := a.Auth.Authenticate(r.Context(), deref(body.Username), deref(body.Pw))
	if errors.Is(err, auth.ErrInvalidCredentials) {
		a.Log.InfoContext(r.Context(), "failed login", "user", deref(body.Username), "client", info.Client, "remote", remoteIP(r))
		errorText(w, http.StatusUnauthorized)
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.writeLogin(w, r, u, auth.Device{ID: info.DeviceID, Name: info.Device, AppName: info.Client, AppVersion: info.Version}, false)
}

// writeLogin issues a token for u on dev and answers with Jellyfin's
// AuthenticationResult (password and QuickConnect logins alike; 12.1.0
// leaves the remote address out of a QuickConnect login's SessionInfo).
func (a *api) writeLogin(w http.ResponseWriter, r *http.Request, u db.User, dev auth.Device, quickConnect bool) {
	token, err := a.Auth.IssueToken(r.Context(), u, dev)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	sess := auth.Session{UserID: u.ID, UserName: u.Name, IsAdmin: u.IsAdmin,
		DeviceID: dev.ID, DeviceName: dev.Name, AppName: dev.AppName, AppVersion: dev.AppVersion}
	ud, err := a.userDto(u)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	info := a.sessionInfo(sess, r)
	if quickConnect {
		info.RemoteEndPoint = nil
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.AuthenticationResult{
		User:        &ud.UserDto,
		SessionInfo: &info,
		AccessToken: &token,
		ServerId:    ptr(a.ServerID.String()),
	})
}

func (a *api) usersMe(w http.ResponseWriter, r *http.Request, s auth.Session) {
	a.writeUser(w, r, s.UserID)
}

// userByID lets users read themselves; reading someone else needs admin.
func (a *api) userByID(w http.ResponseWriter, r *http.Request, s auth.Session) {
	id, err := dto.ParseID(jfapi.URLParam(r, "userId"))
	if err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	if id.UUID() != s.UserID && !s.IsAdmin {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	a.writeUser(w, r, id.UUID())
}

func (a *api) writeUser(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	u, err := a.Queries.GetUserByID(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	ud, err := a.userDto(u)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	jfapi.WriteJSON(w, r, http.StatusOK, ud)
}

func (a *api) listUsers(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	users, err := a.Queries.ListUsers(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		ud, err := a.userDto(u)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		out = append(out, ud)
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}

func (a *api) logout(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if err := a.Auth.Revoke(r.Context(), jfapi.AuthFrom(r.Context()).Token); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// userPolicy is Jellyfin's UserPolicy with blockbustr's own fields, stored
// with it in users.policy. A client that doesn't know them (Jellyfin's own
// dashboard) drops them when it saves a policy.
type userPolicy struct {
	dto.UserPolicy
	// MaxVideoHeight caps the height of the addon versions the user is
	// offered (1080: no 4K); 0 or absent: no cap.
	MaxVideoHeight *int32 `json:",omitempty"`
}

// userDTO is Jellyfin's UserDto with blockbustr's policy fields.
type userDTO struct {
	dto.UserDto
	Policy *userPolicy `json:"Policy,omitempty"`
}

// userDto builds Jellyfin's UserDto: defaults, then the user's stored
// overrides, then the fields the database owns (admin, disabled).
func (a *api) userDto(u db.User) (userDTO, error) {
	var cfg dto.UserConfiguration
	if err := mergeJSON(&cfg, defaultUserConfiguration, u.Configuration); err != nil {
		return userDTO{}, err
	}
	var pol userPolicy
	if err := mergeJSON(&pol, defaultUserPolicy, u.Policy); err != nil {
		return userDTO{}, err
	}
	pol.IsAdministrator, pol.IsDisabled = ptr(u.IsAdmin), ptr(u.IsDisabled)
	hasPassword := u.PasswordHash != nil
	var lastLogin *dto.Time
	if u.LastLoginAt != nil {
		lastLogin = ptr(dto.NewTime(*u.LastLoginAt))
	}
	return userDTO{UserDto: dto.UserDto{
		Name:                      ptr(u.Name),
		ServerId:                  ptr(a.ServerID.String()),
		Id:                        ptr(dto.IDFromUUID(u.ID)),
		HasPassword:               ptr(hasPassword),
		HasConfiguredPassword:     ptr(hasPassword),
		HasConfiguredEasyPassword: ptr(false),
		EnableAutoLogin:           ptr(false),
		LastLoginDate:             lastLogin,
		LastActivityDate:          lastLogin,
		Configuration:             &cfg,
		Policy:                    &pol.UserPolicy,
	}, Policy: &pol}, nil
}

// sessionInfo is the SessionInfo returned with a login. Its shape follows
// 12.1.0's response; live session state (now playing, capabilities) arrives
// with the sessions work (P2.9).
func (a *api) sessionInfo(s auth.Session, r *http.Request) dto.SessionInfoDto {
	return dto.SessionInfoDto{
		Id:       ptr(sessionID(s.DeviceID)),
		ServerId: ptr(a.ServerID.String()),
		UserId:   ptr(dto.IDFromUUID(s.UserID)),
		UserName: ptr(s.UserName),
		PlayState: &dto.PlayerStateInfo{
			CanSeek: ptr(false), IsPaused: ptr(false), IsMuted: ptr(false),
			RepeatMode: ptr(dto.RepeatModeRepeatNone), PlaybackOrder: ptr(dto.PlaybackOrderDefault),
		},
		AdditionalUsers: &[]dto.SessionUserInfo{},
		Capabilities: &dto.ClientCapabilitiesDto{
			PlayableMediaTypes: &[]dto.MediaType{}, SupportedCommands: &[]dto.GeneralCommandType{},
			SupportsMediaControl: ptr(false), SupportsPersistentIdentifier: ptr(true),
		},
		RemoteEndPoint:        ptr(remoteIP(r)),
		PlayableMediaTypes:    &[]dto.MediaType{},
		Client:                ptr(s.AppName),
		DeviceName:            ptr(s.DeviceName),
		DeviceId:              ptr(s.DeviceID),
		ApplicationVersion:    ptr(s.AppVersion),
		LastActivityDate:      ptr(dto.NewTime(time.Now())),
		LastPlaybackCheckIn:   &dto.Time{}, // Jellyfin sends DateTime.MinValue until playback starts
		IsActive:              ptr(true),
		SupportsMediaControl:  ptr(false),
		SupportsRemoteControl: ptr(false),
		NowPlayingQueue:       &[]dto.QueueItem{},
		HasCustomDeviceName:   ptr(false),
		SupportedCommands:     &[]dto.GeneralCommandType{},
	}
}

var sessionNamespace = uuid.MustParse("6c1f6a0e-6d0b-4a8f-9a55-6b1f2f0c9d11")

// sessionID is stable per device, so a client sees the same session id
// across logins (P2.9 builds live sessions on it).
func sessionID(deviceID string) string {
	return dto.IDFromUUID(uuid.NewSHA1(sessionNamespace, []byte(deviceID))).String()
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// mergeJSON decodes defaults into dst and then overrides on top (an empty or
// {} override changes nothing).
func mergeJSON(dst any, defaults, override []byte) error {
	if err := json.Unmarshal(defaults, dst); err != nil {
		return err
	}
	if len(override) == 0 {
		return nil
	}
	return json.Unmarshal(override, dst)
}

// deref is *p, or the zero value for nil.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
