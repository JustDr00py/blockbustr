package dto

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestParseIDAcceptsEverySpelling(t *testing.T) {
	want := "84088e11eb6351255c08507602d79a0f"
	for _, in := range []string{
		"84088e11eb6351255c08507602d79a0f",
		"84088e11-eb63-5125-5c08-507602d79a0f",
		"84088E11-EB63-5125-5C08-507602D79A0F",
		"{84088e11-eb63-5125-5c08-507602d79a0f}",
	} {
		id, err := ParseID(in)
		if err != nil || id.String() != want {
			t.Errorf("ParseID(%q) = %s, %v", in, id, err)
		}
	}
	if id, err := ParseID(""); err != nil || !id.IsZero() {
		t.Errorf("empty: %v %v", id, err)
	}
	if _, err := ParseID("not-a-guid"); err == nil {
		t.Error("expected error")
	}
}

func TestIDJSON(t *testing.T) {
	u := uuid.MustParse("bbbb0000-0000-0000-0000-000000000001")
	type doc struct {
		Id     ID
		Opt    *ID           `json:",omitempty"`
		Byname map[ID]string `json:",omitempty"`
	}
	b, err := json.Marshal(doc{Id: IDFromUUID(u), Byname: map[ID]string{IDFromUUID(u): "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"Id":"bbbb0000000000000000000000000001","Byname":{"bbbb0000000000000000000000000001":"x"}}` {
		t.Errorf("marshal = %s", b)
	}
	var d doc
	if err := json.Unmarshal([]byte(`{"Id":"BBBB0000-0000-0000-0000-000000000001"}`), &d); err != nil || d.Id.UUID() != u {
		t.Errorf("unmarshal = %+v, %v", d, err)
	}
	if err := json.Unmarshal([]byte(`{"Id":"nope"}`), &d); err == nil {
		t.Error("expected error for bad id")
	}
}
