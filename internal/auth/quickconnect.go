package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// QuickConnect (TASKS P2.12, DESIGN §3.2): a device without a login asks
// for a code, a signed-in user approves the code on another device, and the
// first device trades its secret for a token. Requests live in Redis for
// cache.QuickConnectTTL: qc:{secret} holds the state, qc:code:{code} finds
// it from the code.

var (
	// ErrQuickConnectUnknown: no such pending request (expired, used, or
	// never made).
	ErrQuickConnectUnknown = errors.New("auth: unknown quick connect request")
	// ErrQuickConnectPending: the request hasn't been approved yet.
	ErrQuickConnectPending = errors.New("auth: quick connect request not authorized yet")
)

// QuickConnectRequest is one pending request.
type QuickConnectRequest struct {
	Secret        string // 64 hex, what the requesting device polls with
	Code          string // 6 digits, what the user types on the other device
	Device        Device
	DateAdded     time.Time
	Authenticated bool
	UserID        uuid.UUID // who approved it
}

func randomCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// QuickConnectInitiate starts a request for dev.
func (s *Service) QuickConnectInitiate(ctx context.Context, dev Device) (QuickConnectRequest, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return QuickConnectRequest{}, err
	}
	req := QuickConnectRequest{Secret: strings.ToUpper(hex.EncodeToString(b)), Device: dev, DateAdded: time.Now().UTC()}
	for range 10 { // a code in use by another pending request is drawn again
		code, err := randomCode()
		if err != nil {
			return req, err
		}
		var other string
		ok, err := s.cache.GetJSON(ctx, cache.QuickConnectCodeKey(code), &other)
		if err != nil {
			return req, err
		}
		if !ok {
			req.Code = code
			break
		}
	}
	if req.Code == "" {
		return req, errors.New("auth: no free quick connect code")
	}
	if err := s.cache.SetJSON(ctx, cache.QuickConnectCodeKey(req.Code), req.Secret, cache.QuickConnectTTL); err != nil {
		return req, err
	}
	return req, s.cache.SetJSON(ctx, cache.QuickConnectKey(req.Secret), req, cache.QuickConnectTTL)
}

// QuickConnectState returns a request by its secret.
func (s *Service) QuickConnectState(ctx context.Context, secret string) (QuickConnectRequest, error) {
	var req QuickConnectRequest
	ok, err := s.cache.GetJSON(ctx, cache.QuickConnectKey(strings.ToUpper(secret)), &req)
	if err != nil {
		return req, err
	}
	if !ok {
		return req, ErrQuickConnectUnknown
	}
	return req, nil
}

// QuickConnectAuthorize approves the request with code for user.
func (s *Service) QuickConnectAuthorize(ctx context.Context, code string, user uuid.UUID) error {
	var secret string
	ok, err := s.cache.GetJSON(ctx, cache.QuickConnectCodeKey(strings.TrimSpace(code)), &secret)
	if err != nil {
		return err
	}
	if !ok {
		return ErrQuickConnectUnknown
	}
	req, err := s.QuickConnectState(ctx, secret)
	if err != nil {
		return err
	}
	req.Authenticated, req.UserID = true, user
	return s.cache.SetJSON(ctx, cache.QuickConnectKey(req.Secret), req, cache.QuickConnectTTL)
}

// QuickConnectRedeem trades an approved request's secret for its user and
// device, once: the request is gone afterwards.
func (s *Service) QuickConnectRedeem(ctx context.Context, secret string) (db.User, Device, error) {
	req, err := s.QuickConnectState(ctx, secret)
	if err != nil {
		return db.User{}, Device{}, err
	}
	if !req.Authenticated {
		return db.User{}, Device{}, ErrQuickConnectPending
	}
	if err := s.cache.Delete(ctx, cache.QuickConnectKey(req.Secret), cache.QuickConnectCodeKey(req.Code)); err != nil {
		return db.User{}, Device{}, err
	}
	u, err := s.q.GetUserByID(ctx, req.UserID)
	if err != nil {
		return db.User{}, Device{}, err
	}
	if u.IsDisabled {
		return db.User{}, Device{}, ErrInvalidCredentials
	}
	// A login like a password one (Jellyfin records it too).
	if err := s.q.SetUserLastLogin(ctx, u.ID); err != nil {
		return db.User{}, Device{}, err
	}
	u, err = s.q.GetUserByID(ctx, u.ID)
	return u, req.Device, err
}
