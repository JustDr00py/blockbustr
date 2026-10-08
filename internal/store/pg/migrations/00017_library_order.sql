-- The admin's library order and hiding (shown on the Libraries page):
-- every user's libraries come in position order (then by name) unless
-- they set their own (UserConfiguration.OrderedViews); a hidden library
-- leaves every user's views and Latest rows but keeps syncing and stays
-- searchable.
-- +goose Up
ALTER TABLE libraries
    ADD COLUMN position integer NOT NULL DEFAULT 0,
    ADD COLUMN hidden   boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE libraries DROP COLUMN hidden, DROP COLUMN position;
