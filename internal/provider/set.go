package provider

import "sync/atomic"

// Set is the enabled debrid accounts in priority order. It is shared by the
// resolver and the stream collector and replaced as a whole when an admin
// changes an account (TASKS P4.3), so readers never see a half-updated set.
type Set struct {
	p atomic.Pointer[setData]
}

type setData struct {
	byName map[Name]Provider
	order  []Name
}

// NewSet returns a Set of byName, in order (names missing from order come
// last).
func NewSet(byName map[Name]Provider, order []Name) *Set {
	s := &Set{}
	s.Replace(byName, order)
	return s
}

// Replace swaps in a new set of accounts.
func (s *Set) Replace(byName map[Name]Provider, order []Name) {
	d := &setData{byName: map[Name]Provider{}}
	for _, n := range order {
		if p := byName[n]; p != nil {
			if _, dup := d.byName[n]; !dup {
				d.byName[n] = p
				d.order = append(d.order, n)
			}
		}
	}
	for n, p := range byName {
		if _, ok := d.byName[n]; !ok && p != nil {
			d.byName[n] = p
			d.order = append(d.order, n)
		}
	}
	s.p.Store(d)
}

func (s *Set) data() *setData {
	if d := s.p.Load(); d != nil {
		return d
	}
	return &setData{}
}

// Get returns the account for name, or nil.
func (s *Set) Get(name Name) Provider { return s.data().byName[name] }

// Names are the accounts in priority order.
func (s *Set) Names() []Name { return append([]Name(nil), s.data().order...) }

// Ordered are the accounts in priority order.
func (s *Set) Ordered() []Provider {
	d := s.data()
	out := make([]Provider, 0, len(d.order))
	for _, n := range d.order {
		out = append(out, d.byName[n])
	}
	return out
}

// Len is how many accounts there are.
func (s *Set) Len() int { return len(s.data().order) }
