package pg

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

const (
	settingServerID  = "server_id"
	settingStreamKey = "stream_url_key"
)

// EnsureServerID returns the server's permanent id, generating it on first
// start. Clients remember servers by this id (PublicSystemInfo.Id), so it
// must never change.
func EnsureServerID(ctx context.Context, q *db.Queries) (uuid.UUID, error) {
	candidate, err := json.Marshal(uuid.New().String())
	if err != nil {
		return uuid.Nil, err
	}
	if err := q.InsertSettingIfMissing(ctx, db.InsertSettingIfMissingParams{Key: settingServerID, Value: candidate}); err != nil {
		return uuid.Nil, fmt.Errorf("server id: %w", err)
	}
	raw, err := q.GetSetting(ctx, settingServerID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("server id: %w", err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return uuid.Nil, fmt.Errorf("server id: %w", err)
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("server id: stored value %q: %w", s, err)
	}
	return id, nil
}

// EnsureStreamKey returns the key stream URLs are signed with (TASKS
// P3.10), generating it on first start. Kept in the database so signed
// URLs survive restarts and every instance agrees.
func EnsureStreamKey(ctx context.Context, q *db.Queries) ([]byte, error) {
	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	candidate, err := json.Marshal(base64.StdEncoding.EncodeToString(fresh))
	if err != nil {
		return nil, err
	}
	if err := q.InsertSettingIfMissing(ctx, db.InsertSettingIfMissingParams{Key: settingStreamKey, Value: candidate}); err != nil {
		return nil, fmt.Errorf("stream key: %w", err)
	}
	raw, err := q.GetSetting(ctx, settingStreamKey)
	if err != nil {
		return nil, fmt.Errorf("stream key: %w", err)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("stream key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("stream key: stored value unusable")
	}
	return key, nil
}
