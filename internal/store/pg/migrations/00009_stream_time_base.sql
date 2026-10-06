-- Stream fields Jellyfin reports that weren't stored yet (TASKS P1.20):
-- the ffprobe time base ("1/1000" in Matroska, "1/90000" for MP4 video…) and
-- the rest of the Dolby Vision configuration record.

-- +goose Up
ALTER TABLE media_streams
    ADD COLUMN time_base        text,
    ADD COLUMN dv_version_major integer,
    ADD COLUMN dv_version_minor integer,
    ADD COLUMN dv_rpu_present   boolean,
    ADD COLUMN dv_el_present    boolean,
    ADD COLUMN dv_bl_present    boolean;

-- Local files were probed without these fields: forget their etag so the
-- next scan re-probes each once (probe results are derived data).
UPDATE media_sources SET etag = NULL WHERE protocol = 'File';

-- +goose Down
ALTER TABLE media_streams
    DROP COLUMN IF EXISTS time_base,
    DROP COLUMN IF EXISTS dv_version_major,
    DROP COLUMN IF EXISTS dv_version_minor,
    DROP COLUMN IF EXISTS dv_rpu_present,
    DROP COLUMN IF EXISTS dv_el_present,
    DROP COLUMN IF EXISTS dv_bl_present;
