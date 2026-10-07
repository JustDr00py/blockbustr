package resolve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/sysadmin/blockbustr/internal/provider"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// ErrDownloading is returned for a torrent a debrid account is still
// downloading: it was added (or found) and will play once it's done.
var ErrDownloading = errors.New("resolve: torrent is downloading on debrid")

// KnownTorrent is a torrent blockbustr added to a debrid account.
type KnownTorrent struct {
	Provider  provider.Name
	Hash      string
	TorrentID string
	Status    provider.TorrentStatus
}

// TorrentStore remembers the torrents added to debrid accounts, so a hash
// played again is found instead of added again (DESIGN §7.3).
type TorrentStore interface {
	Find(ctx context.Context, hash string) ([]KnownTorrent, error)
	Save(ctx context.Context, t KnownTorrent) error
	Forget(ctx context.Context, p provider.Name, hash string) error
}

// PGTorrents is the TorrentStore in Postgres (debrid_torrents).
type PGTorrents struct{ Q *db.Queries }

// Find implements TorrentStore.
func (s PGTorrents) Find(ctx context.Context, hash string) ([]KnownTorrent, error) {
	rows, err := s.Q.ListDebridTorrents(ctx, hash)
	if err != nil {
		return nil, err
	}
	out := make([]KnownTorrent, len(rows))
	for i, r := range rows {
		out[i] = KnownTorrent{Provider: provider.Name(r.Provider), Hash: r.InfoHash, TorrentID: r.TorrentID, Status: provider.TorrentStatus(r.Status)}
	}
	return out, nil
}

// Save implements TorrentStore.
func (s PGTorrents) Save(ctx context.Context, t KnownTorrent) error {
	return s.Q.UpsertDebridTorrent(ctx, db.UpsertDebridTorrentParams{
		Provider: string(t.Provider), InfoHash: t.Hash, TorrentID: t.TorrentID, Status: string(t.Status),
	})
}

// Forget implements TorrentStore.
func (s PGTorrents) Forget(ctx context.Context, p provider.Name, hash string) error {
	return s.Q.DeleteDebridTorrent(ctx, db.DeleteDebridTorrentParams{Provider: string(p), InfoHash: hash})
}

// Ready reports which of hashes are ready on some account: stream ranking
// counts them as cached (Real-Debrid can't be asked any more).
func (s PGTorrents) Ready(ctx context.Context, hashes []string) map[string]bool {
	out := map[string]bool{}
	got, err := s.Q.ReadyDebridHashes(ctx, hashes)
	if err != nil {
		return out
	}
	for _, h := range got {
		out[h] = true
	}
	return out
}

// instantBudget bounds asking the accounts which one has a torrent cached.
const instantBudget = 2 * time.Second

// torrent resolves an infoHash stream (DESIGN §7.3 step 4): the torrent
// already on an account, else one added to the account that has it cached
// (or the first by priority); then the file (by name, index, or the
// largest video) and its link. A torrent still downloading answers
// ErrDownloading; the account keeps downloading it.
func (r *Resolver) torrent(ctx context.Context, src Source) (Link, time.Duration, error) {
	if len(r.order()) == 0 {
		return Link{}, 0, fmt.Errorf("%w: no debrid account for infoHash streams", ErrNotResolvable)
	}
	if r.Torrents != nil {
		known, err := r.Torrents.Find(ctx, src.InfoHash)
		if err != nil {
			return Link{}, 0, fmt.Errorf("resolve: %w", err)
		}
		slices.SortStableFunc(known, func(a, b KnownTorrent) int { return cmp.Compare(r.rank(a.Provider), r.rank(b.Provider)) })
		for _, k := range known {
			p := r.account(k.Provider)
			if p == nil {
				continue
			}
			t, err := p.Torrent(ctx, k.TorrentID)
			if err != nil {
				// Deleted from the cloud (or the account is failing): add
				// it again below.
				r.warn(ctx, "known debrid torrent unavailable", "provider", k.Provider, "err", err)
				_ = r.Torrents.Forget(ctx, k.Provider, k.Hash)
				continue
			}
			return r.torrentLink(ctx, p, k, t, src)
		}
	}
	v, err, _ := r.flight.Do("add\x00"+src.InfoHash, func() (any, error) {
		// An account that refuses (a revoked key, a full queue, an outage)
		// hands the torrent to the next one.
		var errs []error
		for _, p := range r.addOrder(ctx, src.InfoHash) {
			id, _, err := p.AddMagnet(ctx, "magnet:?xt=urn:btih:"+src.InfoHash)
			if err != nil {
				r.warn(ctx, "debrid account refused a torrent; trying the next", "provider", p.Name(), "err", err)
				errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
				continue
			}
			k := KnownTorrent{Provider: p.Name(), Hash: src.InfoHash, TorrentID: id, Status: provider.StatusDownloading}
			r.save(ctx, k)
			return k, nil
		}
		return nil, fmt.Errorf("resolve: add torrent: %w", errors.Join(errs...))
	})
	if err != nil {
		return Link{}, 0, err
	}
	k := v.(KnownTorrent)
	p := r.account(k.Provider)
	t, err := p.Torrent(ctx, k.TorrentID)
	if err != nil {
		return Link{}, 0, fmt.Errorf("resolve: %s: %w", k.Provider, err)
	}
	return r.torrentLink(ctx, p, k, t, src)
}

// torrentLink records t's status and, when it's ready, returns the file's
// link.
func (r *Resolver) torrentLink(ctx context.Context, p provider.Provider, k KnownTorrent, t provider.Torrent, src Source) (Link, time.Duration, error) {
	k.Status = t.Status
	switch t.Status {
	case provider.StatusError:
		// A dead or failed torrent never becomes playable: drop it so the
		// next try starts over (possibly on another account).
		_ = p.Delete(ctx, k.TorrentID)
		if r.Torrents != nil {
			_ = r.Torrents.Forget(ctx, k.Provider, k.Hash)
		}
		return Link{}, 0, fmt.Errorf("resolve: %s: torrent failed", k.Provider)
	case provider.StatusReady:
	default:
		r.save(ctx, k)
		return Link{}, 0, fmt.Errorf("%w (%s)", ErrDownloading, k.Provider)
	}
	r.save(ctx, k)
	f, ok := pickFile(t.Files, src.FileIdx, src.FileName)
	if !ok {
		return Link{}, 0, fmt.Errorf("%w: %s torrent has no video file", ErrNotResolvable, k.Provider)
	}
	return r.debrid(ctx, Source{Kind: Debrid, Provider: k.Provider, TorrentID: k.TorrentID, FileID: f.ID})
}

func (r *Resolver) save(ctx context.Context, k KnownTorrent) {
	if r.Torrents == nil {
		return
	}
	if err := r.Torrents.Save(ctx, k); err != nil {
		r.warn(ctx, "debrid torrent not remembered", "provider", k.Provider, "err", err)
	}
}

func (r *Resolver) warn(ctx context.Context, msg string, args ...any) {
	if r.Log != nil {
		r.Log.WarnContext(ctx, msg, args...)
	}
}

// order is the accounts in priority order: Order, then any others by name.
func (r *Resolver) order() []provider.Name {
	if r.Accounts != nil {
		return r.Accounts.Names()
	}
	out := make([]provider.Name, 0, len(r.Providers))
	for _, n := range r.Order {
		if r.Providers[n] != nil && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	var rest []provider.Name
	for n, p := range r.Providers {
		if p != nil && !slices.Contains(out, n) {
			rest = append(rest, n)
		}
	}
	slices.Sort(rest)
	return append(out, rest...)
}

func (r *Resolver) rank(n provider.Name) int {
	order := r.order()
	if i := slices.Index(order, n); i >= 0 {
		return i
	}
	return len(order)
}

// addOrder is the accounts to try adding a torrent to: the first (by
// priority) that reports it cached, then the rest by priority.
func (r *Resolver) addOrder(ctx context.Context, hash string) []provider.Provider {
	order := r.order()
	first := -1
	if len(order) > 1 {
		ictx, cancel := context.WithTimeout(ctx, instantBudget)
		defer cancel()
	find:
		for i, n := range order {
			res, err := r.account(n).InstantCheck(ictx, []string{hash})
			if err != nil {
				continue
			}
			for _, ir := range res {
				if ir.Cached && strings.EqualFold(ir.Hash, hash) {
					first = i
					break find
				}
			}
		}
	}
	out := make([]provider.Provider, 0, len(order))
	if first >= 0 {
		out = append(out, r.account(order[first]))
	}
	for i, n := range order {
		if i != first {
			out = append(out, r.account(n))
		}
	}
	return out
}

var videoExts = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".webm": true, ".ts": true, ".m2ts": true, ".mov": true, ".wmv": true, ".mpg": true}

func isVideo(name string) bool { return videoExts[strings.ToLower(path.Ext(name))] }

// pickFile chooses the file a stream means: the one named like the addon's
// filename, else the one at its index (Stremio's fileIdx counts every file
// in torrent order, as the accounts list them once all are selected), else
// the largest video.
func pickFile(files []provider.File, idx int, name string) (provider.File, bool) {
	if name != "" {
		for _, f := range files {
			if strings.EqualFold(path.Base(f.Path), path.Base(name)) {
				return f, true
			}
		}
	}
	if idx >= 0 && idx < len(files) && isVideo(files[idx].Path) {
		return files[idx], true
	}
	var best provider.File
	found := false
	for _, f := range files {
		if isVideo(f.Path) && (!found || f.SizeBytes > best.SizeBytes) {
			best, found = f, true
		}
	}
	return best, found
}
