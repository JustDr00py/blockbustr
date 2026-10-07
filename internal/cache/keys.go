package cache

import (
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Key layout and TTLs from DESIGN §5. Keep every key name here so the layout
// stays reviewable in one place.

// TTLs.
const (
	TokenTTL          = 24 * time.Hour // sliding, refreshed on use
	SessionTTL        = 10 * time.Minute
	LinkTTL           = 4 * time.Hour
	ProbeTTL          = 30 * 24 * time.Hour
	StremioStreamsTTL = 30 * time.Minute
	StremioMetaTTL    = 24 * time.Hour
	StremioSubsTTL    = 6 * time.Hour
	QueryTTL          = 5 * time.Minute // cached query results (q:*), retired by events
	TranscodeLockTTL  = 30 * time.Second
	ScanLockTTL       = 10 * time.Minute
	QuickConnectTTL   = 10 * time.Minute
	SearchTTL         = time.Hour
	PlaySessionTTL    = 24 * time.Hour // a PlaybackInfo decision, read by stream/HLS requests
	StreamSetTTL      = 12 * time.Hour // addon streams offered for a title, refreshed on PlaybackInfo
	StreamBadTTL      = 6 * time.Hour  // a stream choice that failed to play isn't offered again for this long
	StreamPrefetchTTL = time.Minute    // one background collection per title while its details are opened
)

// SessionsKey is the set of active device IDs.
const SessionsKey Key = "sessions"

// EventsChannel is the pub/sub channel for typed events (DESIGN §2).
const EventsChannel = "events"

// TokenKey caches the user/device behind an access token, keyed by its sha256.
func TokenKey(sha []byte) Key { return Key("tok:" + hex.EncodeToString(sha)) }

// DeviceTokensKey is the set of token hashes (hex) cached for a device, so
// revoking a device's tokens can also drop their tok:* cache entries.
func DeviceTokensKey(deviceID string) Key { return Key("devtok:" + deviceID) }

// SessionKey holds a device's now-playing session.
func SessionKey(deviceID string) Key { return Key("sess:" + deviceID) }

// LinkKey caches a resolved CDN URL for a source.
func LinkKey(sourceHash string) Key { return Key("link:" + sourceHash) }

// ProbeKey caches ffprobe output for a source.
func ProbeKey(sourceHash string) Key { return Key("probe:" + sourceHash) }

// StremioStreamsKey caches an addon's stream list for one title.
func StremioStreamsKey(addon, typ, id string) Key {
	return Key("stremio:streams:" + addon + ":" + typ + ":" + id)
}

// StreamSetKey holds the addon streams offered as an item's media sources
// (DESIGN §7.3), so stream requests find the one a client picked.
func StreamSetKey(item string) Key { return Key("streamset:" + item) }

// StremioSubtitlesKey caches an addon's subtitle list for one title.
func StremioSubtitlesKey(addon, typ, id string) Key {
	return Key("stremio:subs:" + addon + ":" + typ + ":" + id)
}

// StreamBadKey holds the stream choices (media source ids) of an item that
// failed to play: blocked by the debrid service, refused by every account.
func StreamBadKey(item string) Key { return Key("streambad:" + item) }

// StreamPrefetchKey guards one title's background stream collection, started
// by opening its details without a remembered set (TASKS P3.7 follow-up).
func StreamPrefetchKey(item string) Key { return Key("lock:streamprefetch:" + item) }

// StremioMetaKey caches an addon's meta for one title.
func StremioMetaKey(addon, typ, id string) Key {
	return Key("stremio:meta:" + addon + ":" + typ + ":" + id)
}

// QueryGenKey holds the query generation: the time (Unix ns) of the last
// library or user-data event (events.Bus.Publish), or of the first read
// after Redis lost it. The q:* result keys embed it, so an event retires
// every cached result at once. A time, not a counter: Redis may evict the
// key (allkeys-lru) while results remain, and a counter starting over
// would bring those back.
func QueryGenKey() Key { return Key("q:gen") }

// QueryKey holds one query result (kind: count, nextup…) for one
// generation; hash identifies the query and its arguments, user included.
func QueryKey(kind string, gen int64, hash string) Key {
	return Key("q:" + kind + ":" + strconv.FormatInt(gen, 10) + ":" + hash)
}

// PlaySessionKey holds the decisions PlaybackInfo made for a play session.
func PlaySessionKey(playSessionID string) Key { return Key("play:" + playSessionID) }

// TranscodeLockKey guards one transcode session across instances.
func TranscodeLockKey(playSessionID string) Key { return Key("lock:transcode:" + playSessionID) }

// ScanLockKey guards one library scan across instances.
func ScanLockKey(libraryID uuid.UUID) Key { return Key("lock:scan:" + libraryID.String()) }

// RateLimitKey is a provider's token bucket (debrid, TMDB).
func RateLimitKey(provider string) Key { return Key("rl:" + provider) }

// QuickConnectKey holds a QuickConnect request's state.
func QuickConnectKey(secret string) Key { return Key("qc:" + secret) }

// QuickConnectCodeKey maps a QuickConnect code to its request's secret.
func QuickConnectCodeKey(code string) Key { return Key("qc:code:" + code) }

// SearchKey caches one remote search source's results (DESIGN §7.4). The term
// is normalised (case, whitespace) so equivalent searches share an entry.
func SearchKey(source, kind, term string) Key {
	return Key("search:" + source + ":" + kind + ":" + strings.Join(strings.Fields(strings.ToLower(term)), " "))
}
