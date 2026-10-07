package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/images"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
)

// BaseItemDto for library items (Movie, Series, Season, Episode). The field
// set follows Jellyfin 12.1.0: a base set always sent per kind, plus the
// optional ItemFields a client asks for in `fields`. Everything a page needs
// is batch-loaded, so a page costs a fixed number of queries whatever its
// size. MediaSources/MediaStreams come with P1.20.

// dtoOptions are the request's DtoOptions.
type dtoOptions struct {
	fields         map[string]bool // lower-cased ItemFields
	enableImages   bool
	imageTypeLimit int             // -1 = unlimited
	imageTypes     map[string]bool // nil = all
	enableUserData bool
}

// dtoOptionsFrom reads fields, enableImages, imageTypeLimit,
// enableImageTypes and enableUserData from the query string.
func dtoOptionsFrom(q jfapi.Query) dtoOptions {
	o := dtoOptions{fields: map[string]bool{}, enableImages: true, imageTypeLimit: -1, enableUserData: true}
	for _, f := range q.List("fields") {
		o.fields[strings.ToLower(f)] = true
	}
	if b, ok := q.Bool("enableImages"); ok {
		o.enableImages = b
	}
	if q.Get("imageTypeLimit") != "" {
		if n, ok := q.Int("imageTypeLimit"); ok {
			o.imageTypeLimit = n
		}
	}
	if ts := q.List("enableImageTypes"); len(ts) > 0 {
		o.imageTypes = map[string]bool{}
		for _, t := range ts {
			o.imageTypes[strings.ToLower(t)] = true
		}
	}
	if b, ok := q.Bool("enableUserData"); ok {
		o.enableUserData = b
	}
	return o
}

func (o dtoOptions) has(f string) bool { return o.fields[f] }

// itemBatch is everything loaded for one page of items.
type itemBatch struct {
	byID     map[uuid.UUID]db.Item // the items and their parents
	images   map[uuid.UUID][]db.Image
	media    map[uuid.UUID]db.MediaSummaryForItemsRow
	userData map[uuid.UUID]*db.UserDatum
	episodes map[uuid.UUID]db.UnplayedEpisodeCountsRow
	children map[uuid.UUID]int64
	genres   map[uuid.UUID][]db.ListGenresForItemsRow
	studios  map[uuid.UUID][]db.ListStudiosForItemsRow
	people   map[uuid.UUID][]db.ListPeopleForItemsRow
	chapters map[uuid.UUID][]db.Chapter
	sources  map[uuid.UUID][]db.MediaSource
	base     string                         // server URL for remote sources' stream URLs
	streams  map[uuid.UUID][]db.MediaStream // by media source
}

func (b *itemBatch) parent(it db.Item) (db.Item, bool) {
	if it.ParentID == nil {
		return db.Item{}, false
	}
	p, ok := b.byID[*it.ParentID]
	return p, ok
}

// seriesAndSeason finds an episode's or season's series and season.
func (b *itemBatch) seriesAndSeason(it db.Item) (series, season *db.Item) {
	p, ok := b.parent(it)
	if !ok {
		return nil, nil
	}
	switch {
	case it.Type == "Season" && p.Type == "Series":
		return &p, nil
	case it.Type == "Episode" && p.Type == "Season":
		if s, ok := b.parent(p); ok && s.Type == "Series" {
			return &s, &p
		}
		return nil, &p
	case it.Type == "Episode" && p.Type == "Series":
		return &p, nil
	}
	return nil, nil
}

func (a *api) loadItemBatch(ctx context.Context, user *uuid.UUID, items []db.Item, o dtoOptions) (*itemBatch, error) {
	b := &itemBatch{byID: map[uuid.UUID]db.Item{}, images: map[uuid.UUID][]db.Image{}, media: map[uuid.UUID]db.MediaSummaryForItemsRow{},
		userData: map[uuid.UUID]*db.UserDatum{}, episodes: map[uuid.UUID]db.UnplayedEpisodeCountsRow{}, children: map[uuid.UUID]int64{},
		genres: map[uuid.UUID][]db.ListGenresForItemsRow{}, studios: map[uuid.UUID][]db.ListStudiosForItemsRow{},
		people: map[uuid.UUID][]db.ListPeopleForItemsRow{}, chapters: map[uuid.UUID][]db.Chapter{},
		sources: map[uuid.UUID][]db.MediaSource{}, streams: map[uuid.UUID][]db.MediaStream{}, base: baseURL(ctx)}
	ids := make([]uuid.UUID, 0, len(items))
	var folders, playable []uuid.UUID
	for _, it := range items {
		b.byID[it.ID] = it
		ids = append(ids, it.ID)
		switch it.Type {
		case "Series", "Season":
			folders = append(folders, it.ID)
		case "Movie", "Episode":
			playable = append(playable, it.ID)
		}
	}
	// Parents, two levels (episode → season → series).
	for range 2 {
		var missing []uuid.UUID
		for _, it := range b.byID {
			if it.ParentID != nil && (it.Type == "Episode" || it.Type == "Season") {
				if _, ok := b.byID[*it.ParentID]; !ok {
					missing = append(missing, *it.ParentID)
				}
			}
		}
		if len(missing) == 0 {
			break
		}
		ps, err := a.Queries.GetItemsByIDs(ctx, missing)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			b.byID[p.ID] = p
		}
	}
	all := make([]uuid.UUID, 0, len(b.byID))
	for id := range b.byID {
		all = append(all, id)
	}
	imgs, err := a.Queries.ListImagesForItems(ctx, all)
	if err != nil {
		return nil, err
	}
	for _, im := range imgs {
		b.images[im.ItemID] = append(b.images[im.ItemID], im)
	}
	if len(playable) > 0 {
		ms, err := a.Queries.MediaSummaryForItems(ctx, playable)
		if err != nil {
			return nil, err
		}
		for _, m := range ms {
			if _, ok := b.media[m.ItemID]; !ok {
				b.media[m.ItemID] = m
			}
		}
		if o.has("mediasources") || o.has("mediastreams") {
			if err := a.loadSources(ctx, b, playable); err != nil {
				return nil, err
			}
			// One title's details list its stream choices as versions; lists
			// never ask the addons.
			if len(items) == 1 {
				if err := a.addStreamChoices(ctx, b, items[0], false, stremio.Prefs{}); err != nil {
					return nil, err
				}
				// None remembered: collect now, waiting a little so the first
				// view usually lists them; a slower addon is announced later.
				if len(b.sources[items[0].ID]) == 0 && a.waitPrefetch(ctx, a.prefetchStreamChoices(ctx, items[0])) {
					if err := a.addStreamChoices(ctx, b, items[0], false, stremio.Prefs{}); err != nil {
						return nil, err
					}
				}
			}
		}
		if o.has("chapters") {
			cs, err := a.Queries.ListChaptersForItems(ctx, playable)
			if err != nil {
				return nil, err
			}
			for _, c := range cs {
				b.chapters[c.ItemID] = append(b.chapters[c.ItemID], c)
			}
		}
	}
	if user != nil && o.enableUserData {
		uds, err := a.Queries.ListUserData(ctx, db.ListUserDataParams{UserID: *user, ItemIds: ids})
		if err != nil {
			return nil, err
		}
		for i := range uds {
			b.userData[uds[i].ItemID] = &uds[i]
		}
	}
	if len(folders) > 0 {
		var uid uuid.UUID
		if user != nil {
			uid = *user
		}
		eps, err := a.Queries.UnplayedEpisodeCounts(ctx, db.UnplayedEpisodeCountsParams{Ids: folders, UserID: uid})
		if err != nil {
			return nil, err
		}
		for _, e := range eps {
			if e.ItemID != nil {
				b.episodes[*e.ItemID] = e
			}
		}
		if o.has("childcount") {
			cs, err := a.Queries.ChildCountsForItems(ctx, folders)
			if err != nil {
				return nil, err
			}
			for _, c := range cs {
				b.children[c.ItemID] = c.N
			}
		}
	}
	if o.has("genres") {
		gs, err := a.Queries.ListGenresForItems(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, g := range gs {
			b.genres[g.ItemID] = append(b.genres[g.ItemID], g)
		}
	}
	if o.has("studios") || o.has("seriesstudio") {
		ss, err := a.Queries.ListStudiosForItems(ctx, all) // episodes show their series' studio
		if err != nil {
			return nil, err
		}
		for _, s := range ss {
			b.studios[s.ItemID] = append(b.studios[s.ItemID], s)
		}
	}
	if o.has("people") {
		ps, err := a.Queries.ListPeopleForItems(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			b.people[p.ItemID] = append(b.people[p.ItemID], p)
		}
	}
	return b, nil
}

func (a *api) loadSources(ctx context.Context, b *itemBatch, items []uuid.UUID) error {
	srcs, err := a.Queries.ListMediaSourcesForItems(ctx, items)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(srcs))
	for i, s := range srcs {
		b.sources[s.ItemID] = append(b.sources[s.ItemID], s)
		ids[i] = s.ID
	}
	if len(ids) == 0 {
		return nil
	}
	sts, err := a.Queries.ListMediaStreamsForSources(ctx, ids)
	if err != nil {
		return err
	}
	for _, st := range sts {
		b.streams[st.MediaSourceID] = append(b.streams[st.MediaSourceID], st)
	}
	return nil
}

// imageSet is an item's artwork as Jellyfin reports it.
type imageSet struct {
	tags      map[string]*string
	backdrops []string
	blurhash  map[string]map[string]*string
	primary   *db.Image
}

func (o dtoOptions) imageSetOf(imgs []db.Image) imageSet {
	s := imageSet{tags: map[string]*string{}, backdrops: []string{}, blurhash: map[string]map[string]*string{}}
	for i := range imgs {
		im := imgs[i]
		if o.imageTypes != nil && !o.imageTypes[strings.ToLower(im.Type)] {
			continue
		}
		if im.Type == "Backdrop" {
			if o.imageTypeLimit >= 0 && len(s.backdrops) >= o.imageTypeLimit {
				continue
			}
			s.backdrops = append(s.backdrops, im.Tag)
		} else {
			if im.Idx != 0 || o.imageTypeLimit == 0 {
				continue
			}
			s.tags[im.Type] = ptr(im.Tag)
			if im.Type == "Primary" {
				s.primary = &imgs[i]
			}
		}
		if im.Blurhash != nil {
			if s.blurhash[im.Type] == nil {
				s.blurhash[im.Type] = map[string]*string{}
			}
			s.blurhash[im.Type][im.Tag] = im.Blurhash
		}
	}
	return s
}

func providerIDs(raw json.RawMessage) map[string]*string {
	var m map[string]string
	_ = json.Unmarshal(raw, &m)
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = ptr(v)
	}
	return out
}

// externalURLs mirrors Jellyfin's IMDb/TMDB links.
func externalURLs(it db.Item, series *db.Item) []dto.ExternalUrl {
	ids := providerIDs(it.ProviderIds)
	out := []dto.ExternalUrl{}
	if v := ids["Imdb"]; v != nil {
		out = append(out, dto.ExternalUrl{Name: ptr("IMDb"), Url: ptr("https://www.imdb.com/title/" + *v)})
	}
	const tmdb = "https://www.themoviedb.org/"
	switch it.Type {
	case "Movie":
		if v := ids["Tmdb"]; v != nil {
			out = append(out, dto.ExternalUrl{Name: ptr("TMDB"), Url: ptr(tmdb + "movie/" + *v)})
		}
	case "Series":
		if v := ids["Tmdb"]; v != nil {
			out = append(out, dto.ExternalUrl{Name: ptr("TMDB"), Url: ptr(tmdb + "tv/" + *v)})
		}
	case "Episode":
		if series == nil || it.ParentIndexNumber == nil || it.IndexNumber == nil {
			break
		}
		if v := providerIDs(series.ProviderIds)["Tmdb"]; v != nil {
			url := tmdb + "tv/" + *v + "/season/" + strconv.Itoa(int(*it.ParentIndexNumber)) + "/episode/" + strconv.Itoa(int(*it.IndexNumber))
			out = append(out, dto.ExternalUrl{Name: ptr("TMDB"), Url: &url})
		}
	}
	return out
}

func isFolderType(t string) bool {
	switch t {
	case "Series", "Season", "CollectionFolder", "Folder", "BoxSet":
		return true
	}
	return false
}

// itemDtos builds BaseItemDtos for items, in order. user may be nil
// (no UserData).
func (a *api) itemDtos(ctx context.Context, user *uuid.UUID, items []db.Item, o dtoOptions) ([]dto.BaseItemDto, error) {
	b, err := a.loadItemBatch(ctx, user, items, o)
	if err != nil {
		return nil, err
	}
	out := make([]dto.BaseItemDto, len(items))
	for i, it := range items {
		out[i] = a.itemDto(b, it, user != nil && o.enableUserData, o)
	}
	return out, nil
}

func (a *api) itemDto(b *itemBatch, it db.Item, withUser bool, o dtoOptions) dto.BaseItemDto {
	id := dto.IDFromUUID(it.ID)
	folder := isFolderType(it.Type)
	d := dto.BaseItemDto{
		Name: ptr(it.Name), ServerId: ptr(a.ServerID.String()), Id: &id, Type: ptr(dto.BaseItemKind(it.Type)),
		IsFolder: ptr(folder), LocationType: ptr(dto.FileSystem), MediaType: ptr(dto.MediaTypeUnknown),
		ProductionYear: it.ProductionYear, CommunityRating: it.CommunityRating, CriticRating: it.CriticRating,
		OfficialRating: it.OfficialRating, RunTimeTicks: it.RuntimeTicks, IndexNumber: it.IndexNumber,
	}
	if it.PremiereDate != nil {
		d.PremiereDate = ptr(dto.NewTime(*it.PremiereDate))
	}
	series, season := b.seriesAndSeason(it)

	// Jellyfin reports the original language only for movies and series.
	if it.Type == "Movie" || it.Type == "Series" {
		d.OriginalLanguage = it.OriginalLanguage
	}

	imgs := o.imageSetOf(b.images[it.ID])
	d.ImageBlurHashes = &imgs.blurhash
	if o.enableImages {
		d.ImageTags, d.BackdropImageTags = &imgs.tags, &imgs.backdrops
	}

	switch it.Type {
	case "Movie", "Episode":
		d.MediaType = ptr(dto.MediaTypeVideo)
		d.VideoType = ptr(dto.VideoTypeVideoFile)
		if m, ok := b.media[it.ID]; ok {
			d.Container = m.Container
			if m.HasSubtitles { // Jellyfin leaves it out when false
				d.HasSubtitles = ptr(true)
			}
			if (o.has("width") || o.has("height")) && m.Width > 0 {
				d.Width, d.Height = ptr(m.Width), ptr(m.Height)
			}
			if o.has("ishd") && m.Width > 0 {
				d.IsHD = ptr(m.Width >= 1260 || m.Height >= 700)
			}
		}
		if o.has("chapters") {
			cs := []dto.ChapterInfo{}
			for _, c := range b.chapters[it.ID] {
				cs = append(cs, dto.ChapterInfo{StartPositionTicks: ptr(c.StartTicks), Name: ptr(c.Name), ImageDateModified: &dto.Time{}})
			}
			d.Chapters = &cs
		}
		if o.has("trickplay") {
			d.Trickplay = &map[string]*map[string]dto.TrickplayInfoDto{}
		}
		if o.has("mediasources") || o.has("mediastreams") {
			srcs := mediaSourceDtos(it, b.sources[it.ID], b.streams, b.base, a.StreamSigner)
			if o.has("mediasources") {
				d.MediaSources = &srcs
			}
			if o.has("mediastreams") {
				d.MediaStreams = srcs[0].MediaStreams
			}
		}
	case "Series":
		// TMDB end dates are only stored for ended or cancelled shows.
		if it.MetadataSource != nil {
			status := "Continuing"
			if it.EndDate != nil {
				status = "Ended"
			}
			d.Status = &status
		}
		d.AirDays = &[]dto.DayOfWeek{}
		if it.EndDate != nil {
			d.EndDate = ptr(dto.NewTime(*it.EndDate))
		}
	}
	if it.Type == "Episode" {
		d.ParentIndexNumber = it.ParentIndexNumber
		if season != nil {
			d.SeasonId, d.SeasonName = ptr(dto.IDFromUUID(season.ID)), ptr(season.Name)
		}
	}
	if series != nil {
		sid := dto.IDFromUUID(series.ID)
		d.SeriesId, d.SeriesName = &sid, ptr(series.Name)
		if o.enableImages {
			a.parentImages(&d, b, o, sid, series, season, it.Type)
		}
	}

	if withUser {
		ud := userDataDto(it.ID, b.userData[it.ID])
		if folder {
			e := b.episodes[it.ID]
			ud.UnplayedItemCount = ptr(int32(e.Unplayed))
			ud.Played = ptr(e.Total > 0 && e.Unplayed == 0)
			ud.PlayedPercentage = ptr(0.0)
			if e.Total > 0 {
				ud.PlayedPercentage = ptr(float64(e.Total-e.Unplayed) * 100 / float64(e.Total))
			}
		}
		if !folder && it.RuntimeTicks != nil && *it.RuntimeTicks > 0 && *ud.PlaybackPositionTicks > 0 {
			ud.PlayedPercentage = ptr(float64(*ud.PlaybackPositionTicks) * 100 / float64(*it.RuntimeTicks))
		}
		d.UserData = &ud
	}
	applyItemFields(&d, b, it, series, imgs, o)
	return d
}

// parentImages fills the series/season artwork fields of seasons and episodes.
// Like Jellyfin, the blurhash of every parent image it points at is added to
// the item's own ImageBlurHashes.
func (a *api) parentImages(d *dto.BaseItemDto, b *itemBatch, o dtoOptions, sid dto.ID, series, season *db.Item, typ string) {
	simgs := o.imageSetOf(b.images[series.ID])
	hashes := *d.ImageBlurHashes
	borrow := func(from imageSet, imgType string, tag *string) {
		if tag == nil {
			return
		}
		if h := from.blurhash[imgType][*tag]; h != nil {
			if hashes[imgType] == nil {
				hashes[imgType] = map[string]*string{}
			}
			hashes[imgType][*tag] = h
		}
	}
	d.SeriesPrimaryImageTag = simgs.tags["Primary"]
	borrow(simgs, "Primary", d.SeriesPrimaryImageTag)
	if len(simgs.backdrops) > 0 {
		d.ParentBackdropItemId, d.ParentBackdropImageTags = &sid, &simgs.backdrops
		for i := range simgs.backdrops {
			borrow(simgs, "Backdrop", &simgs.backdrops[i])
		}
	}
	if t := simgs.tags["Logo"]; t != nil {
		d.ParentLogoItemId, d.ParentLogoImageTag = &sid, t
		borrow(simgs, "Logo", t)
	}
	if t := simgs.tags["Thumb"]; t != nil {
		d.ParentThumbItemId, d.ParentThumbImageTag = &sid, t
		borrow(simgs, "Thumb", t)
	}
	// An episode's nearest ancestor with a poster: the season, else the series.
	if typ != "Episode" {
		return
	}
	if season != nil {
		simg := o.imageSetOf(b.images[season.ID])
		if t := simg.tags["Primary"]; t != nil {
			d.ParentPrimaryImageItemId, d.ParentPrimaryImageTag = ptr(dto.IDFromUUID(season.ID)), t
			borrow(simg, "Primary", t)
			return
		}
	}
	if t := simgs.tags["Primary"]; t != nil {
		d.ParentPrimaryImageItemId, d.ParentPrimaryImageTag = &sid, t
		borrow(simgs, "Primary", t)
	}
}

// applyItemFields adds the optional ItemFields the client asked for.
func applyItemFields(d *dto.BaseItemDto, b *itemBatch, it db.Item, series *db.Item, imgs imageSet, o dtoOptions) {
	folder := isFolderType(it.Type)
	if o.has("candelete") {
		d.CanDelete = ptr(false) // blockbustr never deletes media files
	}
	if o.has("candownload") {
		d.CanDownload = ptr(false)
	}
	if o.has("childcount") && folder {
		d.ChildCount = ptr(int32(b.children[it.ID]))
	}
	if o.has("recursiveitemcount") && folder {
		d.RecursiveItemCount = ptr(int32(b.episodes[it.ID].Total))
	}
	if o.has("cumulativeruntimeticks") && it.Type == "Series" {
		d.CumulativeRunTimeTicks = ptr(b.episodes[it.ID].RuntimeTicks)
	}
	if o.has("datelastmediaadded") && folder {
		d.DateLastMediaAdded = &dto.Time{}
		if t := b.episodes[it.ID].LastAdded; !t.IsZero() {
			d.DateLastMediaAdded = ptr(dto.NewTime(t))
		}
	}
	if o.has("chapters") && folder {
		d.Chapters = &[]dto.ChapterInfo{}
	}
	if o.has("datecreated") {
		d.DateCreated = ptr(dto.NewTime(it.DateCreated))
	}
	if o.has("displaypreferencesid") {
		d.DisplayPreferencesId = ptr(d.Id.String())
	}
	if o.has("etag") {
		d.Etag = ptr(itemEtag(it.ID, it.DateModified, it.Etag))
	}
	if o.has("externalurls") {
		d.ExternalUrls = ptr(externalURLs(it, series))
	}
	if o.has("genres") {
		names, pairs := []string{}, []dto.NameGuidPair{}
		for _, g := range b.genres[it.ID] {
			names = append(names, g.Name)
			pairs = append(pairs, dto.NameGuidPair{Name: ptr(g.Name), Id: ptr(dto.IDFromUUID(g.ID))})
		}
		d.Genres, d.GenreItems = &names, &pairs
	}
	if o.has("studios") {
		pairs := []dto.NameGuidPair{}
		for _, s := range b.studios[it.ID] {
			pairs = append(pairs, dto.NameGuidPair{Name: ptr(s.Name), Id: ptr(dto.IDFromUUID(s.ID))})
		}
		d.Studios = &pairs
	}
	if o.has("seriesstudio") && series != nil {
		if ss := b.studios[series.ID]; len(ss) > 0 {
			d.SeriesStudio = ptr(ss[0].Name)
		}
	}
	if o.has("originaltitle") && (it.Type == "Movie" || it.Type == "Series") {
		d.OriginalTitle = it.OriginalTitle
		if d.OriginalTitle == nil {
			d.OriginalTitle = ptr(it.Name)
		}
	}
	if o.has("overview") {
		d.Overview = it.Overview
		if foundBySearch(it) {
			d.Overview = ptr(strings.TrimSpace(deref(it.Overview) + "\n\n" + notInLibrary))
		}
	}
	if o.has("parentid") && it.ParentID != nil {
		d.ParentId = ptr(dto.IDFromUUID(*it.ParentID))
	}
	if o.has("path") {
		d.Path = it.Path
	}
	if o.has("people") {
		ps := []dto.BaseItemPerson{}
		for _, p := range b.people[it.ID] {
			bp := dto.BaseItemPerson{Name: ptr(p.Name), Id: ptr(dto.IDFromUUID(p.ID)), Role: p.Role, Type: ptr(dto.PersonKind(p.Kind))}
			if p.ImageUrl != nil {
				bp.PrimaryImageTag = ptr(images.Tag(*p.ImageUrl))
			}
			ps = append(ps, bp)
		}
		d.People = &ps
	}
	if o.has("playaccess") {
		d.PlayAccess = ptr(dto.PlayAccessFull)
	}
	if o.has("productionlocations") && it.Type == "Movie" {
		d.ProductionLocations = &[]string{}
	}
	if o.has("providerids") {
		d.ProviderIds = ptr(providerIDs(it.ProviderIds))
	}
	if o.has("primaryimageaspectratio") && imgs.primary != nil {
		d.PrimaryImageAspectRatio = ptr(aspectRatio(*imgs.primary, it.Type))
	}
	if o.has("sortname") {
		d.SortName = ptr(it.SortName)
		if it.ForcedSortName != nil {
			d.SortName = it.ForcedSortName
		}
	}
	if o.has("taglines") {
		tl := []string{}
		if it.Tagline != nil {
			tl = append(tl, *it.Tagline)
		}
		d.Taglines = &tl
	}
	if o.has("tags") {
		d.Tags = &[]string{}
	}
	if o.has("remotetrailers") {
		d.RemoteTrailers = &[]dto.MediaUrl{}
	}
	if o.has("settings") {
		d.LockData, d.LockedFields = ptr(false), &[]dto.MetadataField{}
	}
	if o.has("enablemediasourcedisplay") {
		d.EnableMediaSourceDisplay = ptr(true)
	}
	if o.has("localtrailercount") {
		d.LocalTrailerCount = ptr(int32(0))
	}
	if o.has("specialfeaturecount") {
		d.SpecialFeatureCount = ptr(int32(0))
	}
}

// aspectRatio is width/height of the poster, or Jellyfin's default for the
// kind when the size isn't known yet.
func aspectRatio(im db.Image, typ string) float64 {
	if im.Width != nil && im.Height != nil && *im.Height > 0 {
		return float64(*im.Width) / float64(*im.Height)
	}
	if typ == "Episode" {
		return 16.0 / 9
	}
	return 2.0 / 3
}

// notInLibrary ends a search-found title's overview (DESIGN §7.4 step 6),
// so it's clear it plays from the addons, not the library.
const notInLibrary = "Not in your library · streams from your addons"

// foundBySearch reports whether it is a title of the hidden discover
// library: an addon title with no library folder above it (catalog titles
// sit under their library's CollectionFolder).
func foundBySearch(it db.Item) bool {
	return it.SourceKind == "stremio" && it.ParentID == nil && (it.Type == "Movie" || it.Type == "Series")
}
