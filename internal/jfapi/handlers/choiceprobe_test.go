package handlers

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// matchboxStreams is a French "VFF" release: French audio and forced French
// subtitles are its defaults.
func matchboxStreams() []db.MediaStream {
	return []db.MediaStream{
		{Idx: 0, Type: "Video"},
		{Idx: 1, Type: "Audio", Language: ptr("fre"), IsDefault: true},
		{Idx: 2, Type: "Audio", Language: ptr("eng")},
		{Idx: 3, Type: "Audio", Language: ptr("fre"), Title: ptr("AD")},
		{Idx: 4, Type: "Audio", Language: ptr("eng"), Title: ptr("AD")},
		{Idx: 5, Type: "Subtitle", Language: ptr("fre"), Title: ptr("Forced"), IsDefault: true, IsForced: true},
		{Idx: 6, Type: "Subtitle", Language: ptr("fre")},
		{Idx: 7, Type: "Subtitle", Language: ptr("fre"), IsHearingImpaired: true},
		{Idx: 8, Type: "Subtitle", Language: ptr("eng")},
	}
}

func TestPreferAudioClearsOtherSubtitleDefaults(t *testing.T) {
	streams := matchboxStreams()
	preferAudio(streams, "eng")
	for _, st := range streams {
		switch {
		case st.Type == "Audio" && st.IsDefault != (st.Idx == 2):
			t.Errorf("audio %d default = %v", st.Idx, st.IsDefault)
		case st.Type == "Subtitle" && st.IsDefault:
			t.Errorf("subtitle %d is still the default", st.Idx)
		}
	}
	if !streams[5].IsForced {
		t.Error("the forced flag was cleared")
	}
}

func TestPreferAudioKeepsPreferredSubtitleDefault(t *testing.T) {
	streams := matchboxStreams()
	streams[5].IsDefault, streams[8].IsDefault = false, true
	preferAudio(streams, "eng")
	if !streams[8].IsDefault {
		t.Error("the English subtitle lost its default")
	}
}

func TestApplyProbesRanksForeignStartLast(t *testing.T) {
	fre, eng, unprobed := uuid.New(), uuid.New(), uuid.New()
	probes := map[uuid.UUID]choiceProbe{
		fre: {Streams: matchboxStreams()},
		eng: {Streams: []db.MediaStream{
			{Idx: 0, Type: "Video"},
			{Idx: 1, Type: "Audio", Language: ptr("eng"), IsDefault: true},
			{Idx: 2, Type: "Audio", Language: ptr("fre")},
		}},
	}
	order := func(lang string) []uuid.UUID {
		sources := []db.MediaSource{{ID: fre}, {ID: eng}, {ID: unprobed}}
		applyProbes(sources, map[uuid.UUID][]db.MediaStream{}, probes, lang)
		var ids []uuid.UUID
		for _, s := range sources {
			ids = append(ids, s.ID)
		}
		return ids
	}
	if got, want := order("eng"), []uuid.UUID{eng, unprobed, fre}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v (eng, unprobed, fre)", got, want)
	}
	if got, want := order(""), []uuid.UUID{fre, eng, unprobed}; !slices.Equal(got, want) {
		t.Errorf("without a language, order = %v, want %v", got, want)
	}
}
