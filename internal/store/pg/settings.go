package pg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

const settingServerID = "server_id"

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
