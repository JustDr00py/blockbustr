package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Access is what a user's policy lets them see (TASKS P4.1): which
// libraries, and which parental ratings. The zero value restricts nothing.
// Item queries apply the Access in their context (WithAccess), so browsing,
// details, search and playback agree.
type Access struct {
	// Folders are the library folders (CollectionFolder item ids, as
	// clients see libraries) the user may browse; nil means all.
	Folders []uuid.UUID `json:",omitempty"`
	// MaxRating is the highest parental rating score allowed (Jellyfin's
	// scale: G 0, PG 10, PG-13 13, R 17, NC-17 18); nil means no limit.
	MaxRating *int `json:",omitempty"`
	// BlockUnrated lists the item types (Movie, Series) hidden when they
	// have no rating Access can score.
	BlockUnrated []string `json:",omitempty"`
}

// Restricted reports whether a limits anything.
func (a Access) Restricted() bool {
	return a.Folders != nil || a.MaxRating != nil || len(a.BlockUnrated) > 0
}

// AccessFromPolicy reads a stored Jellyfin UserPolicy (users.policy holds
// only the keys an admin set) into an Access.
func AccessFromPolicy(policy []byte) Access {
	var p struct {
		EnableAllFolders  *bool
		EnabledFolders    []string
		MaxParentalRating *int
		BlockUnratedItems []string
	}
	if len(policy) == 0 || json.Unmarshal(policy, &p) != nil {
		return Access{}
	}
	var a Access
	if p.EnableAllFolders != nil && !*p.EnableAllFolders {
		a.Folders = []uuid.UUID{}
		for _, f := range p.EnabledFolders {
			if id, err := uuid.Parse(f); err == nil {
				a.Folders = append(a.Folders, id)
			}
		}
	}
	a.MaxRating = p.MaxParentalRating
	a.BlockUnrated = p.BlockUnratedItems
	return a
}

type accessKey struct{}

// WithAccess returns ctx carrying a for the item queries made with it.
func WithAccess(ctx context.Context, a Access) context.Context {
	return context.WithValue(ctx, accessKey{}, a)
}

// AccessFrom is the Access in ctx (none: unrestricted).
func AccessFrom(ctx context.Context) Access {
	a, _ := ctx.Value(accessKey{}).(Access)
	return a
}

// RatingScores are Jellyfin's parental rating scores for the ratings TMDB
// reports (US film and TV). Other ratings count as unrated.
var RatingScores = map[string]int{
	"APPROVED": 0, "G": 0, "E": 0, "EC": 0, "TV-G": 0, "TV-Y": 0,
	"TV-Y7": 7, "TV-Y7-FV": 7,
	"PG": 10, "TV-PG": 10,
	"PG-13": 13, "T": 13, "TV-14": 14,
	"R": 17, "M": 17, "TV-MA": 17,
	"NC-17": 18, "AO": 18, "X": 18, "XXX": 18,
}

// ratingScoreSQL scores the rating expression rating; NULL when it isn't a
// known rating. The ratings are fixed strings, not user input.
func ratingScoreSQL(rating string) string {
	keys := make([]string, 0, len(RatingScores))
	for k := range RatingScores {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("(CASE upper(" + rating + ")")
	for _, k := range keys {
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", k, RatingScores[k])
	}
	b.WriteString(" END)")
	return b.String()
}

// effectiveRating is an item's rating or, for a season or episode, its
// series' (TMDB rates series, not episodes).
const effectiveRating = `coalesce(i.official_rating,
  (SELECT p.official_rating FROM items p WHERE p.id = i.parent_id),
  (SELECT s.official_rating FROM items p JOIN items s ON s.id = p.parent_id WHERE p.id = i.parent_id))`

// apply adds a's conditions to b (over items i, libraries l). Libraries
// and seasons are never hidden by rating: their content is.
func (a Access) apply(b *sqlBuilder) {
	if a.Folders != nil {
		b.and("l.id IN (SELECT f.library_id FROM items f WHERE f.type = 'CollectionFolder' AND f.id = ANY(" + b.arg(a.Folders) + "))")
	}
	if a.MaxRating == nil && len(a.BlockUnrated) == 0 {
		return
	}
	score := ratingScoreSQL(effectiveRating)
	var conds []string
	if a.MaxRating != nil {
		conds = append(conds, fmt.Sprintf("(%s IS NULL OR %s <= %s)", score, score, b.arg(*a.MaxRating)))
	}
	if len(a.BlockUnrated) > 0 {
		conds = append(conds, fmt.Sprintf("NOT (%s IS NULL AND lower(i.type) = ANY(%s))", score, b.arg(lowerAll(blockedTypes(a.BlockUnrated)))))
	}
	b.and("(i.type IN ('CollectionFolder', 'Season') OR (" + strings.Join(conds, " AND ") + "))")
}

// blockedTypes are the item types an UnratedItem list hides; Series covers
// its episodes too.
func blockedTypes(list []string) []string {
	var out []string
	for _, t := range list {
		out = append(out, t)
		if strings.EqualFold(t, "Series") {
			out = append(out, "Episode")
		}
	}
	return out
}
