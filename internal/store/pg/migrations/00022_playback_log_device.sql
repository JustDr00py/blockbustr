-- The device a play is on, so a client that asks for a new play session
-- mid-play (Void does on every track change) keeps one row per play:
-- MergePlayback joins a new session to the device's running play of the
-- item.

-- +goose Up
ALTER TABLE playback_log ADD COLUMN device_id text NOT NULL DEFAULT '';
CREATE INDEX playback_log_device ON playback_log (user_id, item_id, device_id, last_seen_at DESC);

-- +goose Down
DROP INDEX playback_log_device;
ALTER TABLE playback_log DROP COLUMN device_id;
