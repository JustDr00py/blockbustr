package jfapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode"

	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
)

// camelName converts a PascalCase property name exactly like .NET's
// JsonNamingPolicy.CamelCase: lower-case the leading run of capitals, except
// the last one when it starts the next word ("IPAddress" → "ipAddress",
// "ServerId" → "serverId", "ID" → "id").
func camelName(s string) string {
	r := []rune(s)
	if len(r) == 0 || !unicode.IsUpper(r[0]) {
		return s
	}
	for i := range r {
		if i == 1 && !unicode.IsUpper(r[i]) {
			break
		}
		hasNext := i+1 < len(r)
		if i > 0 && hasNext && !unicode.IsUpper(r[i+1]) {
			if unicode.IsSpace(r[i+1]) {
				r[i] = unicode.ToLower(r[i])
			}
			break
		}
		r[i] = unicode.ToLower(r[i])
	}
	return string(r)
}

// camelJSON rewrites the property names in a PascalCase JSON document to
// camelCase while keeping dictionary keys (ImageTags, ProviderIds, …; see
// dto.MapProperties) as they are, which is what Jellyfin does. Key order and
// number formatting are preserved.
func camelJSON(src []byte) ([]byte, error) {
	type frame struct {
		object    bool
		wantKey   bool
		keepKeys  bool // keys are dictionary keys, not property names
		depth     int  // dictionary levels remaining, including this one
		elemDepth int  // for arrays: depth handed to each element
		n         int
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var out bytes.Buffer
	out.Grow(len(src))
	var stack []*frame
	pending := 0 // dictionary depth for the next value

	valueDone := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].wantKey = true
		}
	}
	writeString := func(s string) error {
		b, err := json.Marshal(s)
		out.Write(b)
		return err
	}

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}

		if top != nil && top.object && top.wantKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:len(stack)-1]
				out.WriteByte('}')
				valueDone()
				continue
			}
			key, _ := tok.(string)
			if top.n > 0 {
				out.WriteByte(',')
			}
			top.n++
			if top.keepKeys {
				pending = top.depth - 1
			} else {
				pending = dto.MapProperties[key]
				key = camelName(key)
			}
			if err := writeString(key); err != nil {
				return nil, err
			}
			out.WriteByte(':')
			top.wantKey = false
			continue
		}

		if top != nil && !top.object {
			if d, ok := tok.(json.Delim); ok && d == ']' {
				stack = stack[:len(stack)-1]
				out.WriteByte(']')
				valueDone()
				continue
			}
			if top.n > 0 {
				out.WriteByte(',')
			}
			top.n++
			pending = top.elemDepth
		}

		switch t := tok.(type) {
		case json.Delim:
			if t == '{' {
				stack = append(stack, &frame{object: true, wantKey: true, keepKeys: pending > 0, depth: pending})
				out.WriteByte('{')
			} else {
				stack = append(stack, &frame{elemDepth: pending})
				out.WriteByte('[')
			}
			pending = 0
			continue // completion is handled when the container closes
		case string:
			if err := writeString(t); err != nil {
				return nil, err
			}
		case json.Number:
			out.WriteString(t.String())
		case bool:
			if t {
				out.WriteString("true")
			} else {
				out.WriteString("false")
			}
		case nil:
			out.WriteString("null")
		}
		pending = 0
		valueDone()
	}
	if len(stack) > 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return out.Bytes(), nil
}
