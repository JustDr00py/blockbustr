-- DESIGN §4: probe results and artwork. Remote sources are probed lazily, so
-- probed_at stays NULL until first playback.

-- +goose Up
CREATE TABLE media_sources (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    item_id      uuid        NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    container    text,
    size         bigint,     -- real remote Content-Length for .strm, never the .strm file size (DESIGN §8.3a)
    bitrate      int,
    path_or_url  text        NOT NULL,
    protocol     text        NOT NULL CHECK (protocol IN ('File', 'Http')),
    is_remote    boolean     NOT NULL DEFAULT false,
    probed_at    timestamptz,
    probe_error  text
);
CREATE INDEX media_sources_item_idx ON media_sources (item_id);

CREATE TABLE media_streams (
    media_source_id uuid    NOT NULL REFERENCES media_sources (id) ON DELETE CASCADE,
    idx             int     NOT NULL,
    type            text    NOT NULL CHECK (type IN ('Video', 'Audio', 'Subtitle', 'EmbeddedImage', 'Data', 'Lyric')),
    codec           text,
    language        text,
    title           text,
    is_default      boolean NOT NULL DEFAULT false,
    is_forced       boolean NOT NULL DEFAULT false,
    is_external     boolean NOT NULL DEFAULT false,
    width           int,
    height          int,
    bitrate         int,
    channels        int,
    channel_layout  text,
    video_range     text,   -- SDR, HDR10, DOVI, HLG…
    profile         text,
    level           real,
    pixel_format    text,
    delivery_url    text,   -- external subtitle files
    PRIMARY KEY (media_source_id, idx)
);

CREATE TABLE images (
    item_id    uuid NOT NULL REFERENCES items (id) ON DELETE CASCADE,
    type       text NOT NULL CHECK (type IN ('Primary', 'Art', 'Backdrop', 'Banner', 'Logo', 'Thumb', 'Disc',
                                             'Box', 'Screenshot', 'Menu', 'Chapter', 'BoxRear', 'Profile')),
    idx        int  NOT NULL DEFAULT 0,
    source_url text,         -- remote artwork (TMDB), fetched lazily
    local_path text,         -- local/cached file
    tag        text NOT NULL, -- cache-busting hash sent to clients as ImageTags
    blurhash   text,
    width      int,
    height     int,
    PRIMARY KEY (item_id, type, idx),
    CHECK (source_url IS NOT NULL OR local_path IS NOT NULL)
);

-- +goose Down
DROP TABLE IF EXISTS images;
DROP TABLE IF EXISTS media_streams;
DROP TABLE IF EXISTS media_sources;
