package jfapi

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestQuery(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/Items?ParentId=p1&parentid=p2&fields=Overview,Genres&Fields=People&IncludeItemTypes=&Recursive=True&isFavorite=nope&Limit=20&startIndex=x", nil)
	q := QueryOf(r)
	if q.Get("PARENTID") != "p1" || !q.Has("includeitemtypes") || q.Has("SearchTerm") {
		t.Errorf("Get/Has: %+v", q)
	}
	if got := q.List("FIELDS"); !reflect.DeepEqual(got, []string{"Overview", "Genres", "People"}) {
		t.Errorf("List = %v", got)
	}
	if got := q.List("IncludeItemTypes"); got != nil {
		t.Errorf("empty list = %v", got)
	}
	enc := QueryOf(httptest.NewRequestWithContext(t.Context(), "GET", "/x?SearchTerm=mortal+kombat%20II&bad=%zz", nil))
	if enc.Get("searchterm") != "mortal kombat II" || enc.Has("bad") {
		t.Errorf("unescaping: %q %v", enc.Get("searchterm"), enc.Has("bad"))
	}
	if v, ok := q.Bool("recursive"); !v || !ok {
		t.Error("Bool true")
	}
	if _, ok := q.Bool("IsFavorite"); ok {
		t.Error("invalid bool should not be ok")
	}
	if n, ok := q.Int("limit"); n != 20 || !ok {
		t.Error("Int")
	}
	if _, ok := q.Int("StartIndex"); ok {
		t.Error("invalid int should not be ok")
	}
}
