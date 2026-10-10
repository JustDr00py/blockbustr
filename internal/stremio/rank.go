package stremio

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Info is what a stream's labels say about it. Addons put this in free
// text (Torrentio: name "Torrentio\n4k HDR", title "release\n👤 23 💾 15.98
// GB ⚙️ 1337x\nMulti Audio / 🇫🇷"), so every field is best effort and zero
// when unknown.
type Info struct {
	Height int // 2160, 1080, 720, 576, 480
	HDR    bool
	DV     bool // Dolby Vision
	// DVOnly: Dolby Vision with no HDR10 base layer named (a WEB-DL's
	// profile 5), which shows purple and green on a display without DV.
	// "DV HDR10", hybrids and remuxes (profile 7) carry an HDR10 base.
	DVOnly    bool
	Codec     string   // "hevc", "av1", "h264", "vp9"
	Size      int64    // bytes
	Seeders   int      // torrents only
	Group     string   // release group ("FLUX")
	Languages []string // ISO 639-1 from flags, plus "multi" for multi-audio
	Cached    bool     // ready: on a debrid account, or a direct URL nobody marked uncached
	Debrid    bool     // known cached on a debrid account (marker or check), not just a URL
	Uncached  bool     // the addon says its debrid service doesn't have it
	Cam       bool     // CAM/TS/screener rip
	Remux     bool
	Audio     string        // "Atmos TrueHD 7.1", "DD+ 5.1"; "" when unlabelled
	Duration  time.Duration // the addon's runtime ("⏱️ 2h:32m:0s"), for when the title has none
}

var (
	reHeight  = regexp.MustCompile(`(?i)\b(2160|1080|720|576|480)[pi]\b`)
	re4K      = regexp.MustCompile(`(?i)\b(4k|uhd)\b`)
	reFHD     = regexp.MustCompile(`(?i)\bfhd\b`)
	reHDR     = regexp.MustCompile(`(?i)\b(hdr10\+?|hdr)\b`)
	reDV      = regexp.MustCompile(`(?i)\b(dv|dovi|dolby vision)\b`)
	reHybrid  = regexp.MustCompile(`(?i)\bhybrid\b`)
	reHEVC    = regexp.MustCompile(`(?i)\b(x265|h 265|h265|hevc)\b`)
	reAV1     = regexp.MustCompile(`(?i)\bav1\b`)
	reH264    = regexp.MustCompile(`(?i)\b(x264|h 264|h264|avc)\b`)
	reVP9     = regexp.MustCompile(`(?i)\bvp9\b`)
	reSize    = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*(tb|gb|mb|tib|gib|mib)\b`)
	reSeeders = regexp.MustCompile(`👤\s*(\d+)`)
	reCam     = regexp.MustCompile(`(?i)\b(cam|camrip|hdcam|ts|hdts|telesync|tc|telecine|scr|screener|dvdscr)\b`)
	reRemux   = regexp.MustCompile(`(?i)\bremux\b`)
	reMulti   = regexp.MustCompile(`(?i)\b(multi|dual audio|dual-audio)\b`)
	// Audio, on the raw text: separators matter ("DDP5.1", "DTS-HD MA").
	reAtmos    = regexp.MustCompile(`(?i)\batmos\b`)
	reAudio    = regexp.MustCompile(`(?i)\b(true[- ]?hd|dts[-: .]?x|dts[- .]?hd(?:[- .]?ma)?|dts|ddp|dd\+|e-?ac-?3|dd|ac-?3|aac|flac|opus)(?:\d|\b)`)
	reChannels = regexp.MustCompile(`(?:^|\D)([1-9]\.[0-2])(?:\D|$)`)
	// "⏱️ 2h:32m:0s", "2h 32m", "1h:05m"; minutes alone ("95m") too.
	reDuration = regexp.MustCompile(`(?i)(?:\b(\d{1,2})\s*h[:\s]*)?\b(\d{1,3})\s*m(?:in)?\b(?:[:\s]*(\d{1,2})\s*s\b)?`)
	// The release group trails the release name: "…x265-FLUX.mkv", "…-FLUX",
	// "… - YIFY". A tracker tag may follow ("-PiRaTeS[TGx]").
	reGroup    = regexp.MustCompile(`-\s?([A-Za-z0-9]{2,20})(?:\.[A-Za-z0-9]{2,4})?$`)
	reTrackTag = regexp.MustCompile(`\[[^\]]*\]$`)
	// Debrid markers: "[RD+]", "⚡", or "[RD download]" when not cached.
	reCached   = regexp.MustCompile(`\[(?:RD|AD|PM|DL|TB|OC|ED|PK)\+\]|⚡`)
	reUncached = regexp.MustCompile(`(?i)\[(?:RD|AD|PM|DL|TB|OC|ED|PK) download\]`)
)

// flagLanguages maps Torrentio's flags to ISO 639-1. English-only
// releases usually carry no flag at all.
var flagLanguages = map[string]string{
	"🇬🇧": "en", "🇺🇸": "en", "🇫🇷": "fr", "🇩🇪": "de", "🇪🇸": "es", "🇲🇽": "es",
	"🇮🇹": "it", "🇵🇹": "pt", "🇧🇷": "pt", "🇷🇺": "ru", "🇺🇦": "uk", "🇵🇱": "pl",
	"🇳🇱": "nl", "🇨🇿": "cs", "🇭🇺": "hu", "🇷🇴": "ro", "🇹🇷": "tr", "🇬🇷": "el",
	"🇸🇪": "sv", "🇳🇴": "no", "🇩🇰": "da", "🇫🇮": "fi", "🇯🇵": "ja", "🇰🇷": "ko",
	"🇨🇳": "zh", "🇹🇼": "zh", "🇮🇳": "hi", "🇸🇦": "ar", "🇮🇱": "he", "🇮🇩": "id",
	"🇹🇭": "th", "🇻🇳": "vi", "🇱🇹": "lt", "🇱🇻": "lv", "🇪🇪": "et", "🇧🇬": "bg",
	"🇷🇸": "sr", "🇭🇷": "hr", "🇸🇰": "sk", "🇸🇮": "sl", "🇲🇾": "ms", "🇵🇭": "tl",
}

// wordSep makes release-name separators spaces, so \b matches "x265" in
// "1080p.x265-FLUX".
var wordSep = strings.NewReplacer(".", " ", "_", " ")

// Parse reads s's labels: the name, the details and the filename together.
// The release group comes from the filename, or else the details' first
// line (the release name).
func Parse(s Stream) Info {
	details := s.Details()
	words := wordSep.Replace(s.Name + "\n" + details + "\n" + s.Hints.Filename)

	var in Info
	if m := reHeight.FindStringSubmatch(words); m != nil {
		in.Height, _ = strconv.Atoi(m[1])
	} else if re4K.MatchString(words) {
		in.Height = 2160
	} else if reFHD.MatchString(words) {
		in.Height = 1080 // AIOStreams' "FHD"
	}
	in.HDR = reHDR.MatchString(words)
	in.DV = reDV.MatchString(words)
	switch {
	case reHEVC.MatchString(words):
		in.Codec = "hevc"
	case reAV1.MatchString(words):
		in.Codec = "av1"
	case reVP9.MatchString(words):
		in.Codec = "vp9"
	case reH264.MatchString(words):
		in.Codec = "h264"
	}
	in.Size = s.Hints.VideoSize
	if in.Size == 0 {
		in.Size = parseSize(details)
	}
	if m := reSeeders.FindStringSubmatch(details); m != nil {
		in.Seeders, _ = strconv.Atoi(m[1])
	}
	in.Group = releaseGroup(s.Hints.Filename, details)
	in.Languages = languages(s.Name + "\n" + details)
	// The addon's own markers win: a debrid-configured addon (Torrentio
	// with a key, AIOStreams) gives every stream a URL, and an "[RD
	// download]" one starts a download and plays a placeholder video.
	in.Debrid = reCached.MatchString(s.Name + " " + details)
	in.Uncached = !in.Debrid && reUncached.MatchString(s.Name+" "+details)
	in.Cached = in.Debrid || (s.URL != "" && !in.Uncached)
	// Only the release name decides CAM: "ts" elsewhere is noise.
	in.Cam = reCam.MatchString(wordSep.Replace(firstLine(details))) || reCam.MatchString(wordSep.Replace(s.Hints.Filename))
	in.Remux = reRemux.MatchString(words)
	in.DVOnly = in.DV && !in.HDR && !in.Remux && !reHybrid.MatchString(words)
	raw := s.Name + "\n" + details + "\n" + s.Hints.Filename
	in.Audio = parseAudio(raw)
	in.Duration = parseDuration(s.Name + "\n" + details)
	return in
}

// audioLabels name the codecs reAudio finds.
var audioLabels = []struct{ prefix, label string }{
	{"true", "TrueHD"}, {"dts-x", "DTS:X"}, {"dtsx", "DTS:X"}, {"dts:x", "DTS:X"}, {"dts x", "DTS:X"}, {"dts.x", "DTS:X"},
	{"dts", "DTS"}, {"ddp", "DD+"}, {"dd+", "DD+"}, {"eac", "DD+"}, {"e-ac", "DD+"}, {"dd", "DD"}, {"ac", "DD"},
	{"aac", "AAC"}, {"flac", "FLAC"}, {"opus", "Opus"},
}

// parseAudio is the best audio a release names: Atmos, the codec and the
// channels ("Atmos TrueHD 7.1").
func parseAudio(text string) string {
	var parts []string
	if reAtmos.MatchString(text) {
		parts = append(parts, "Atmos")
	}
	if m := reAudio.FindStringSubmatch(text); m != nil {
		c := strings.ToLower(m[1])
		for _, l := range audioLabels {
			if strings.HasPrefix(c, l.prefix) {
				label := l.label
				if label == "DTS" && strings.Contains(strings.ReplaceAll(c, "-", ""), "hd") {
					label = "DTS-HD"
					if strings.HasSuffix(c, "ma") {
						label = "DTS-HD MA"
					}
				}
				parts = append(parts, label)
				break
			}
		}
	}
	if m := reChannels.FindStringSubmatchIndex(text); m != nil {
		// Not a size ("2.0 GB").
		rest := strings.TrimSpace(strings.ToLower(text[m[3]:]))
		if !strings.HasPrefix(rest, "gb") && !strings.HasPrefix(rest, "mb") && !strings.HasPrefix(rest, "tb") && !strings.HasPrefix(rest, "gib") {
			parts = append(parts, text[m[2]:m[3]])
		}
	}
	return strings.Join(parts, " ")
}

// parseDuration reads a runtime label; 0 when there's none or it's not
// plausibly a whole title (under 10 minutes).
func parseDuration(text string) time.Duration {
	m := reDuration.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	h, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	sec, _ := strconv.Atoi(m[3])
	d := time.Duration(h)*time.Hour + time.Duration(mins)*time.Minute + time.Duration(sec)*time.Second
	if d < 10*time.Minute {
		return 0
	}
	return d
}

// Bitrate is the stream's average bits/s from its size over runtime (the
// title's, else the addon's own duration); 0 when either is unknown.
func (in Info) Bitrate(runtime time.Duration) int64 {
	if runtime <= 0 {
		runtime = in.Duration
	}
	if in.Size <= 0 || runtime <= 0 {
		return 0
	}
	return int64(float64(in.Size) * 8 / runtime.Seconds())
}

// Average bitrates no release of that class exceeds: Blu-ray caps at 54 Mbps
// with audio, UHD Blu-ray at 144. An addon's size that implies more
// (MediaFusion sends a whole season pack's size for one episode) is not
// the file's, so it is treated as unknown instead of shown and ranked on.
const (
	maxPlausibleBitrateHD  = 60_000_000  // 1080p and below
	maxPlausibleBitrateUHD = 160_000_000 // above 1080p, or height unknown
)

// dropImplausibleSize forgets in.Size when it can't be the title's file
// over runtime (or the addon's own duration): no size, no bitrate estimate.
func (in *Info) dropImplausibleSize(runtime time.Duration) {
	limit := int64(maxPlausibleBitrateUHD)
	if in.Height > 0 && in.Height <= 1080 {
		limit = maxPlausibleBitrateHD
	}
	if in.Bitrate(runtime) > limit {
		in.Size = 0
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var sizeUnits = map[string]float64{"mb": 1 << 20, "mib": 1 << 20, "gb": 1 << 30, "gib": 1 << 30, "tb": 1 << 40, "tib": 1 << 40}

func parseSize(s string) int64 {
	m := reSize.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
	if err != nil {
		return 0
	}
	return int64(n * sizeUnits[strings.ToLower(m[2])])
}

func releaseGroup(filename, details string) string {
	for _, cand := range []string{filename, firstLine(details)} {
		cand = strings.TrimSpace(cand)
		if i := strings.LastIndexByte(cand, '/'); i >= 0 {
			cand = cand[i+1:]
		}
		cand = reTrackTag.ReplaceAllString(cand, "")
		if m := reGroup.FindStringSubmatch(cand); m != nil {
			return m[1]
		}
	}
	return ""
}

func languages(text string) []string {
	var out []string
	add := func(l string) {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	if reMulti.MatchString(text) {
		add("multi")
	}
	runes := []rune(text)
	for i := 0; i+1 < len(runes); i++ {
		if isRegional(runes[i]) && isRegional(runes[i+1]) {
			if l, ok := flagLanguages[string(runes[i:i+2])]; ok {
				add(l)
			}
			i++
		}
	}
	return out
}

func isRegional(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// Prefs are what the ranking weighs streams against: the client's
// abilities (from its DeviceProfile) and the server's configuration.
type Prefs struct {
	MaxHeight  int           // resolution cap; 0 = none
	MaxBitrate int64         // bits/s; 0 = none
	MaxSize    int64         // bytes; 0 = none. The picker drops versions over it
	HEVC, AV1  bool          // the client decodes these itself
	HDR        bool          // the client shows HDR
	DV         bool          // …and Dolby Vision itself (profile 5), not just its HDR10 base
	Runtime    time.Duration // the title's, to turn a size into a bitrate
	Languages  []string      // preferred audio, ISO 639-1
	Allow      []string      // release groups to favour
	Deny       []string      // release groups never offered
}

// Score weights. Being playable right away outweighs everything else;
// then the picture; then how well it fits the client.
const (
	scoreCached          = 1000
	scoreAllowGroup      = 100
	scoreLangMatch       = 80
	scoreLangMulti       = 40
	scoreLangMiss        = -300
	scoreLangExtra       = 20 // a wanted language among others
	scoreCodecMiss       = -150
	scoreHDRMiss         = -50
	scoreDVOnlyMiss      = -150 // like a codec miss: equal versions with an HDR10 base win, cached and height still decide
	scoreOverBudget      = -200 // per multiple of the client's bitrate cap
	scoreOverBudgetFloor = -800 // …clamped, so cached still dominates however far over
	scoreOverHeight      = -250
	maxSeederScore       = 10 // seeders only break ties
)

var heightScore = map[int]int{2160: 400, 1080: 300, 720: 200, 576: 120, 480: 100, 0: 150}

// Score rates one stream for p; higher is better. ok is false when the
// stream must not be offered at all: a denied release group, or a CAM /
// telesync / screener rip (never offered, by user decision 2026-10-07).
func Score(in Info, p Prefs) (score int, ok bool) {
	if in.Cam || (in.Group != "" && containsFold(p.Deny, in.Group)) {
		return 0, false
	}
	if in.Cached {
		score += scoreCached
	}
	score += heightScore[in.Height]
	if p.MaxHeight > 0 && in.Height > p.MaxHeight {
		score += scoreOverHeight
	}
	if (in.Codec == "hevc" && !p.HEVC) || (in.Codec == "av1" && !p.AV1) {
		score += scoreCodecMiss
	}
	if (in.HDR || in.DV) && !p.HDR {
		score += scoreHDRMiss
	}
	if in.DVOnly && !p.DV {
		score += scoreDVOnlyMiss
	}
	if bitrate := in.Bitrate(p.Runtime); p.MaxBitrate > 0 && bitrate > 0 {
		// The penalty grows with the multiple of the cap: a stream's
		// transcode cost is its input bitrate (decode is software), so
		// when every version is over budget the nearest one above the cap
		// becomes the default and is the transcode input, not the biggest.
		// The floor keeps scoreCached dominant: a cached stream however
		// far over still outranks an uncached in-budget one (≤590 at the
		// top of every other score, against 1000+400−800).
		if ratio := float64(bitrate) / float64(p.MaxBitrate); ratio > 1 {
			score += max(int(float64(scoreOverBudget)*ratio), scoreOverBudgetFloor)
		}
	}
	if in.Group != "" && containsFold(p.Allow, in.Group) {
		score += scoreAllowGroup
	}
	score += langScore(in.Languages, p.Languages)
	score += min(in.Seeders/20, maxSeederScore)
	return score, true
}

// langScore: an unflagged release is usually English-only, so it only
// misses when English isn't wanted.
func langScore(have, want []string) int {
	if len(want) == 0 {
		return 0
	}
	if len(have) == 0 || (len(have) == 1 && have[0] == "multi") {
		have = append(slices.Clone(have), "en")
	}
	for _, w := range want {
		if slices.Contains(have, strings.ToLower(w)) {
			// A release in the wanted language only beats a multi-language
			// one, whose default audio is often another language.
			for _, h := range have {
				if h != "multi" && !containsFold(want, h) {
					return scoreLangMatch - scoreLangExtra
				}
			}
			return scoreLangMatch
		}
	}
	if slices.Contains(have, "multi") {
		return scoreLangMulti
	}
	return scoreLangMiss
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(v string) bool { return strings.EqualFold(v, s) })
}

// Ranked is an offer with its labels read and its score.
type Ranked struct {
	Offer
	Info  Info
	Score int
}

// Rank scores offers for p and returns the playable ones, best first.
// Duplicates (the same torrent file or URL from two addons) keep the first
// copy, which comes from the higher-priority addon; equal scores keep the
// offers' order.
func Rank(offers []Offer, p Prefs) []Ranked {
	seen := map[string]bool{}
	out := make([]Ranked, 0, len(offers))
	for _, o := range offers {
		if !o.Stream.Playable() {
			continue
		}
		key := "url:" + o.Stream.URL
		if o.Stream.InfoHash != "" {
			key = "bt:" + o.Stream.InfoHash + "/"
			if o.Stream.FileIdx != nil {
				key += strconv.Itoa(*o.Stream.FileIdx)
			}
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		in := Parse(o.Stream)
		in.dropImplausibleSize(p.Runtime)
		if o.Cached {
			in.Cached, in.Debrid, in.Uncached = true, true, false
		}
		score, ok := Score(in, p)
		if !ok {
			continue
		}
		out = append(out, Ranked{Offer: o, Info: in, Score: score})
	}
	slices.SortStableFunc(out, func(a, b Ranked) int { return cmp.Compare(b.Score, a.Score) })
	return out
}
