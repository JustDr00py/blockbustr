// Package auth is blockbustr's identity layer (DESIGN §3.2): password
// checks, access tokens, and resolving a token to its user and device. It
// knows nothing about HTTP; jfapi/handlers maps it onto Jellyfin's API.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/store/pg"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

var (
	// ErrInvalidCredentials covers unknown users, wrong passwords and disabled
	// accounts alike, so a failed login reveals nothing.
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	// ErrNoSession means the token is unknown, revoked, or its user disabled.
	ErrNoSession = errors.New("auth: no valid session")
)

// Device is the client a token is issued to, taken from the request's
// MediaBrowser authorization fields.
type Device struct {
	ID, Name, AppName, AppVersion string
}

// Session is what a valid token resolves to. It is cached in Redis, so keep
// it small and serialisable.
type Session struct {
	UserID     uuid.UUID
	UserName   string
	IsAdmin    bool
	DeviceID   string
	DeviceName string
	AppName    string
	AppVersion string
	// Access, NoPlayback, NoDownload and NoTranscode come from the user's
	// policy (P4.1): what they may see, and whether they may play, download
	// or have video transcoded. MaxHeight (MaxVideoHeight, blockbustr's
	// own) caps the addon versions they're offered; 0: none.
	Access      pg.Access `json:",omitempty"`
	NoPlayback  bool      `json:",omitempty"`
	NoDownload  bool      `json:",omitempty"`
	NoTranscode bool      `json:",omitempty"`
	MaxHeight   int       `json:",omitempty"`
}

// policySession fills s's policy fields from a stored users.policy.
func policySession(s Session, policy []byte) Session {
	s.Access = pg.AccessFromPolicy(policy)
	var p struct {
		EnableMediaPlayback, EnableContentDownloading, EnableVideoPlaybackTranscoding *bool
		MaxVideoHeight                                                                int
	}
	if json.Unmarshal(policy, &p) == nil {
		s.NoPlayback = p.EnableMediaPlayback != nil && !*p.EnableMediaPlayback
		s.NoDownload = p.EnableContentDownloading != nil && !*p.EnableContentDownloading
		s.NoTranscode = p.EnableVideoPlaybackTranscoding != nil && !*p.EnableVideoPlaybackTranscoding
		s.MaxHeight = max(p.MaxVideoHeight, 0)
	}
	return s
}

// Service implements login and token handling over Postgres and Redis.
type Service struct {
	q     *db.Queries
	cache *cache.Cache
	log   *slog.Logger
}

// New returns a Service.
func New(q *db.Queries, c *cache.Cache, log *slog.Logger) *Service {
	return &Service{q: q, cache: c, log: log}
}

// Authenticate checks a name/password pair (names are case-insensitive) and
// records the login time.
func (s *Service) Authenticate(ctx context.Context, name, password string) (db.User, error) {
	u, err := s.q.GetUserByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.User{}, ErrInvalidCredentials
	}
	if err != nil {
		return db.User{}, err
	}
	if u.IsDisabled || !CheckPassword(u.PasswordHash, password) {
		return db.User{}, ErrInvalidCredentials
	}
	if err := s.q.SetUserLastLogin(ctx, u.ID); err != nil {
		return db.User{}, err
	}
	return s.q.GetUserByID(ctx, u.ID)
}

// IssueToken creates an access token for u on dev. As in Jellyfin, earlier
// tokens for the same device are revoked first, so a device has one session.
func (s *Service) IssueToken(ctx context.Context, u db.User, dev Device) (string, error) {
	if dev.ID == "" {
		return "", errors.New("auth: device id required")
	}
	if err := s.q.UpsertDevice(ctx, db.UpsertDeviceParams{
		ID: dev.ID, UserID: u.ID, Name: dev.Name, AppName: dev.AppName, AppVersion: dev.AppVersion,
	}); err != nil {
		return "", fmt.Errorf("auth: device: %w", err)
	}
	if err := s.revokeDevice(ctx, dev.ID); err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b) // 32 hex chars, like Jellyfin's tokens
	sum := hashToken(token)
	if err := s.q.CreateAccessToken(ctx, db.CreateAccessTokenParams{TokenSha: sum, UserID: u.ID, DeviceID: dev.ID}); err != nil {
		return "", fmt.Errorf("auth: token: %w", err)
	}
	s.cacheSession(ctx, sum, policySession(Session{
		UserID: u.ID, UserName: u.Name, IsAdmin: u.IsAdmin,
		DeviceID: dev.ID, DeviceName: dev.Name, AppName: dev.AppName, AppVersion: dev.AppVersion,
	}, u.Policy))
	return token, nil
}

// Resolve maps a token to its session: Redis first (sliding TTL), Postgres
// on a miss.
func (s *Service) Resolve(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	sum := hashToken(token)
	var sess Session
	if ok, err := s.cache.GetJSONEx(ctx, cache.TokenKey(sum), &sess, cache.TokenTTL); err != nil {
		s.log.WarnContext(ctx, "token cache read failed; using database", "err", err)
	} else if ok {
		return sess, nil
	}
	row, err := s.q.GetTokenSession(ctx, sum)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	if err := s.q.TouchAccessToken(ctx, sum); err != nil {
		return Session{}, err
	}
	sess = policySession(Session{
		UserID: row.UserID, UserName: row.UserName, IsAdmin: row.IsAdmin,
		DeviceID: row.DeviceID, DeviceName: row.DeviceName, AppName: row.AppName, AppVersion: row.AppVersion,
	}, row.Policy)
	s.cacheSession(ctx, sum, sess)
	return sess, nil
}

// Revoke signs one token out.
func (s *Service) Revoke(ctx context.Context, token string) error {
	sum := hashToken(token)
	if _, err := s.q.RevokeAccessToken(ctx, sum); err != nil {
		return err
	}
	return s.cache.Delete(ctx, cache.TokenKey(sum))
}

// revokeDevice signs every token on a device out, in Postgres and in the
// token cache (via the device's set of cached token hashes).
func (s *Service) revokeDevice(ctx context.Context, deviceID string) error {
	if _, err := s.q.RevokeDeviceTokens(ctx, deviceID); err != nil {
		return fmt.Errorf("auth: revoke device: %w", err)
	}
	return s.dropCached(ctx, deviceID)
}

// InvalidateUser drops the user's cached sessions, so the next request
// reads their changed name, admin flag or policy (tokens stay valid).
func (s *Service) InvalidateUser(ctx context.Context, userID uuid.UUID) error {
	devices, err := s.q.ActiveUserDevices(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: user devices: %w", err)
	}
	for _, d := range devices {
		if err := s.dropCached(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// RevokeUser signs the user out on every device (disabled or deleted).
func (s *Service) RevokeUser(ctx context.Context, userID uuid.UUID) error {
	devices, err := s.q.RevokeUserTokens(ctx, userID)
	if err != nil {
		return fmt.Errorf("auth: revoke user: %w", err)
	}
	for _, d := range devices {
		if err := s.dropCached(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// dropCached deletes a device's cached sessions (via its set of cached
// token hashes).
func (s *Service) dropCached(ctx context.Context, deviceID string) error {
	hashes, err := s.cache.SetMembers(ctx, cache.DeviceTokensKey(deviceID))
	if err != nil {
		return fmt.Errorf("auth: revoke device cache: %w", err)
	}
	keys := []cache.Key{cache.DeviceTokensKey(deviceID)}
	for _, h := range hashes {
		if sum, err := hex.DecodeString(h); err == nil {
			keys = append(keys, cache.TokenKey(sum))
		}
	}
	return s.cache.Delete(ctx, keys...)
}

// cacheSession stores a resolved session. Failures only cost a database
// lookup next time, so they are logged, not returned.
func (s *Service) cacheSession(ctx context.Context, sum []byte, sess Session) {
	if err := s.cache.SetJSON(ctx, cache.TokenKey(sum), sess, cache.TokenTTL); err != nil {
		s.log.WarnContext(ctx, "token cache write failed", "err", err)
		return
	}
	if err := s.cache.AddToSet(ctx, cache.DeviceTokensKey(sess.DeviceID), cache.TokenTTL, hex.EncodeToString(sum)); err != nil {
		s.log.WarnContext(ctx, "device token index write failed", "err", err)
	}
}

// Bootstrap makes sure the configured administrator exists with the
// configured password: it is created if missing, otherwise its password is
// reset (lost-password recovery). With no configured admin it only warns
// when there are no users at all, since then nobody can sign in.
func (s *Service) Bootstrap(ctx context.Context, name, password string) error {
	if name == "" {
		n, err := s.q.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			s.log.WarnContext(ctx, "no users exist; set BLOCKBUSTR_ADMIN_USERNAME and BLOCKBUSTR_ADMIN_PASSWORD to create an administrator")
		}
		return nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	u, err := s.q.GetUserByName(ctx, name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := s.q.CreateUser(ctx, db.CreateUserParams{Name: name, PasswordHash: &hash, IsAdmin: true}); err != nil {
			return fmt.Errorf("auth: create admin: %w", err)
		}
		s.log.InfoContext(ctx, "created administrator", "user", name)
		return nil
	case err != nil:
		return err
	}
	if err := s.q.ResetAdminCredentials(ctx, db.ResetAdminCredentialsParams{ID: u.ID, PasswordHash: &hash}); err != nil {
		return fmt.Errorf("auth: reset admin: %w", err)
	}
	s.log.InfoContext(ctx, "administrator credentials applied from config", "user", u.Name)
	return nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
