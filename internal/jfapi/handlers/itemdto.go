package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// BaseItemDto building (DESIGN §3.5). Field sets follow what Jellyfin 12.1.0
// sends for each item kind, including the many always-present empty lists
// (`Genres: []`, `People: []`, …) that clients rely on. P1.18 covers library
// views (CollectionFolder); P1.20 extends this to every item type.

// rootFolderID is the server's top-level folder, the ParentId of every
// library. It is derived from the server id, so it's stable but distinct per server.
func rootFolderID(server dto.ID) dto.ID {
	return dto.IDFromUUID(uuid.NewSHA1(server.UUID(), []byte("root-folder")))
}

// itemEtag is a 32-hex change tag, like Jellyfin's.
func itemEtag(id uuid.UUID, modified time.Time, etag *string) string {
	h := sha256.New()
	h.Write(id[:])
	h.Write([]byte(modified.UTC().Format(time.RFC3339Nano)))
	if etag != nil {
		h.Write([]byte(*etag))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// userDataDto is UserItemDataDto for one item; ud may be nil (never played).
// Jellyfin keys user data by the dashed item GUID and repeats the id in N form.
func userDataDto(itemID uuid.UUID, ud *db.UserDatum) dto.UserItemDataDto {
	out := dto.UserItemDataDto{
		Key: itemID.String(), ItemId: ptr(dto.IDFromUUID(itemID)),
		PlaybackPositionTicks: ptr(int64(0)), PlayCount: ptr(int32(0)), IsFavorite: ptr(false), Played: ptr(false),
	}
	if ud == nil {
		return out
	}
	out.PlaybackPositionTicks, out.PlayCount = ptr(ud.PlaybackPositionTicks), ptr(ud.PlayCount)
	out.IsFavorite, out.Played = ptr(ud.IsFavorite), ptr(ud.Played)
	if ud.LastPlayedAt != nil {
		out.LastPlayedDate = ptr(dto.NewTime(*ud.LastPlayedAt))
	}
	if ud.Rating != nil {
		out.Rating = ptr(float64(*ud.Rating))
	}
	return out
}

// folderView describes a library's top-level folder for the DTO builders.
type folderView struct {
	row        db.ListCollectionFoldersRow
	childCount int64
	userData   *db.UserDatum
}

// collectionFolderDto is a library as /UserViews (withUser) or
// /Library/MediaFolders returns it.
func (a *api) collectionFolderDto(f folderView, withUser bool) dto.BaseItemDto {
	id := dto.IDFromUUID(f.row.ID)
	path := ""
	if len(f.row.Paths) > 0 {
		path = f.row.Paths[0]
	}
	d := dto.BaseItemDto{
		Name:                     ptr(f.row.Name),
		ServerId:                 ptr(a.ServerID.String()),
		Id:                       &id,
		Etag:                     ptr(itemEtag(f.row.ID, f.row.DateModified, f.row.Etag)),
		DateCreated:              ptr(dto.NewTime(f.row.DateCreated)),
		CanDelete:                ptr(false),
		CanDownload:              ptr(false),
		SortName:                 ptr(f.row.SortName),
		ExternalUrls:             &[]dto.ExternalUrl{},
		Path:                     &path,
		EnableMediaSourceDisplay: ptr(true),
		Taglines:                 &[]string{},
		Genres:                   &[]string{},
		RemoteTrailers:           &[]dto.MediaUrl{},
		ProviderIds:              &map[string]*string{},
		IsFolder:                 ptr(true),
		ParentId:                 ptr(rootFolderID(a.ServerID)),
		Type:                     ptr(dto.BaseItemKindCollectionFolder),
		People:                   &[]dto.BaseItemPerson{},
		Studios:                  &[]dto.NameGuidPair{},
		GenreItems:               &[]dto.NameGuidPair{},
		LocalTrailerCount:        ptr(int32(0)),
		SpecialFeatureCount:      ptr(int32(0)),
		DisplayPreferencesId:     ptr(id.String()),
		Tags:                     &[]string{},
		CollectionType:           ptr(dto.CollectionType(f.row.Kind)),
		ImageTags:                &map[string]*string{},
		BackdropImageTags:        &[]string{},
		ImageBlurHashes:          &map[string]map[string]*string{},
		Chapters:                 &[]dto.ChapterInfo{},
		LocationType:             ptr(dto.FileSystem),
		MediaType:                ptr(dto.MediaTypeUnknown),
		LockedFields:             &[]dto.MetadataField{},
		LockData:                 ptr(false),
	}
	if withUser {
		ud := userDataDto(f.row.ID, f.userData)
		d.UserData = &ud
		d.ChildCount = ptr(int32(f.childCount))
		d.PlayAccess = ptr(dto.PlayAccessFull)
		d.DateLastMediaAdded = &dto.Time{} // 12.1.0 sends DateTime.MinValue for libraries
	}
	return d
}
