package cache

import (
	"encoding/hex"
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
	QueryTTL          = 5 * time.Minute // Latest / NextUp results, purged on events
	TranscodeLockTTL  = 30 * time.Second
	ScanLockTTL       = 10 * time.Minute
	QuickConnectTTL   = 10 * time.Minute
	SearchTTL         = time.Hour
	PlaySessionTTL    = 24 * time.Hour // a PlaybackInfo decision, read by stream/HLS requests
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

// StremioMetaKey caches an addon's meta for one title.
func StremioMetaKey(addon, typ, id string) Key {
	return Key("stremio:meta:" + addon + ":" + typ + ":" + id)
}

// LatestKey caches /Items/Latest for a user and parent.
func LatestKey(userID, parentID uuid.UUID) Key {
	return Key("q:latest:" + userID.String() + ":" + parentID.String())
}

// NextUpKey caches /Shows/NextUp for a user.
func NextUpKey(userID uuid.UUID) Key { return Key("q:nextup:" + userID.String()) }

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

// SearchKey caches one remote search source's results (DESIGN §7.4). The term
// is normalised (case, whitespace) so equivalent searches share an entry.
func SearchKey(source, kind, term string) Key {
	return Key("search:" + source + ":" + kind + ":" + strings.Join(strings.Fields(strings.ToLower(term)), " "))
}
