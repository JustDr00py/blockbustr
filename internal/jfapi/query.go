package jfapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Query reads query parameters case-insensitively, as ASP.NET binds them:
// clients send "ParentId", "parentId" and "parentid" interchangeably.
type Query struct{ v map[string][]string }

// QueryOf parses r's query string. Values keep their order in the URL even
// when the same name appears in different casings ("fields=…&Fields=…").
func QueryOf(r *http.Request) Query {
	q := Query{v: map[string][]string{}}
	for _, pair := range strings.Split(r.URL.RawQuery, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		key, err1 := url.QueryUnescape(k)
		val, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			continue // like url.ParseQuery, skip malformed pairs
		}
		lk := strings.ToLower(key)
		q.v[lk] = append(q.v[lk], val)
	}
	return q
}

// Has reports whether name is present (even if empty).
func (q Query) Has(name string) bool {
	_, ok := q.v[strings.ToLower(name)]
	return ok
}

// Get returns the first value for name, or "".
func (q Query) Get(name string) string {
	if vs := q.v[strings.ToLower(name)]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// List returns every value for name, splitting comma-separated lists
// ("Fields=Overview,Genres" and repeated "fields=…" both work) and dropping
// empty entries.
func (q Query) List(name string) []string {
	var out []string
	for _, v := range q.v[strings.ToLower(name)] {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// Bool parses name as a case-insensitive true/false; ok is false if it is
// missing or not a boolean.
func (q Query) Bool(name string) (v, ok bool) {
	switch strings.ToLower(q.Get(name)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// Int parses name as an integer; ok is false if it is missing or invalid.
func (q Query) Int(name string) (int, bool) {
	v, err := strconv.Atoi(q.Get(name))
	return v, err == nil
}
