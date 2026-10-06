package jfapi

import (
	"regexp"
	"strings"
)

// canonicalizer makes routing case-insensitive without touching parameter
// values. Jellyfin is ASP.NET, so clients freely mix "/Items" and "/items"
// (Findroid fetches images from "/items/{id}/Images/Primary"). chi matches
// case-sensitively, so before routing we rewrite only the *literal* parts of
// the path to the casing they were registered with; IDs, file names and
// enum values in parameter positions are passed through untouched.
type canonicalizer struct{ root *canonNode }

type canonNode struct {
	literals map[string]*canonEdge // lower-case segment → edge
	patterns []*canonEdge          // segments containing parameters; mixed before bare
	wildcard bool                  // a "*" route ends here
	terminal bool
}

type canonEdge struct {
	parts []segPart      // literal/parameter pieces of the registered segment
	re    *regexp.Regexp // case-insensitive matcher for parameter segments
	next  *canonNode
}

type segPart struct {
	lit   string
	param bool
}

func newCanonicalizer() *canonicalizer { return &canonicalizer{root: &canonNode{}} }

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// add registers a chi pattern such as "/Videos/{id}/stream.{container}".
func (c *canonicalizer) add(pattern string) {
	n := c.root
	for _, seg := range splitPath(pattern) {
		if seg == "*" {
			n.wildcard = true
			return
		}
		parts := parseSegment(seg)
		if len(parts) == 1 && !parts[0].param {
			if n.literals == nil {
				n.literals = map[string]*canonEdge{}
			}
			key := strings.ToLower(seg)
			e := n.literals[key]
			if e == nil {
				e = &canonEdge{parts: parts, next: &canonNode{}}
				n.literals[key] = e
			}
			n = e.next
			continue
		}
		e := n.findPattern(seg)
		if e == nil {
			e = &canonEdge{parts: parts, re: segmentRegexp(parts), next: &canonNode{}}
			n.patterns = append(n.patterns, e)
			// Segments with a literal part ("stream.{container}") are more
			// specific than a bare "{id}", so try them first.
			sortPatterns(n.patterns)
		}
		n = e.next
	}
	n.terminal = true
}

func (n *canonNode) findPattern(seg string) *canonEdge {
	for _, e := range n.patterns {
		if joinParts(e.parts) == seg {
			return e
		}
	}
	return nil
}

func sortPatterns(es []*canonEdge) {
	hasLit := func(e *canonEdge) bool {
		for _, p := range e.parts {
			if !p.param {
				return true
			}
		}
		return false
	}
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && hasLit(es[j]) && !hasLit(es[j-1]); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
}

func parseSegment(seg string) []segPart {
	var parts []segPart
	for seg != "" {
		open := strings.IndexByte(seg, '{')
		if open < 0 {
			parts = append(parts, segPart{lit: seg})
			break
		}
		if open > 0 {
			parts = append(parts, segPart{lit: seg[:open]})
		}
		end := strings.IndexByte(seg[open:], '}')
		if end < 0 { // malformed; treat the rest as literal
			parts = append(parts, segPart{lit: seg[open:]})
			break
		}
		parts = append(parts, segPart{lit: seg[open : open+end+1], param: true})
		seg = seg[open+end+1:]
	}
	return parts
}

func joinParts(parts []segPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.lit)
	}
	return b.String()
}

func segmentRegexp(parts []segPart) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i, p := range parts {
		switch {
		case !p.param:
			b.WriteString("(?i:" + regexp.QuoteMeta(p.lit) + ")")
		case i == len(parts)-1:
			b.WriteString("(.+)")
		default:
			b.WriteString("(.+?)")
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// canonical returns path with registered literal segments in their canonical
// casing, and whether a registered route matched at all.
func (c *canonicalizer) canonical(path string) (string, bool) {
	segs := splitPath(path)
	out := make([]string, len(segs))
	if !c.root.match(segs, 0, out) {
		return path, false
	}
	return "/" + strings.Join(out, "/"), true
}

func (n *canonNode) match(segs []string, i int, out []string) bool {
	if i == len(segs) {
		return n.terminal
	}
	seg := segs[i]
	if e := n.literals[strings.ToLower(seg)]; e != nil {
		out[i] = e.parts[0].lit
		if e.next.match(segs, i+1, out) {
			return true
		}
	}
	for _, e := range n.patterns {
		m := e.re.FindStringSubmatch(seg)
		if m == nil {
			continue
		}
		var b strings.Builder
		g := 1
		for _, p := range e.parts {
			if p.param {
				b.WriteString(m[g])
				g++
			} else {
				b.WriteString(p.lit)
			}
		}
		out[i] = b.String()
		if e.next.match(segs, i+1, out) {
			return true
		}
	}
	if n.wildcard {
		copy(out[i:], segs[i:])
		return true
	}
	return false
}
