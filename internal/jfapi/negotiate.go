package jfapi

import (
	"mime"
	"sort"
	"strconv"
	"strings"
)

// responseFormat is the JSON casing a client asked for (DESIGN §3.1).
type responseFormat struct {
	camel       bool
	contentType string
}

const (
	contentTypeJSON       = "application/json; charset=utf-8"
	contentTypeCamelJSON  = `application/json; profile="CamelCase"; charset=utf-8`
	contentTypePascalJSON = `application/json; profile="PascalCase"; charset=utf-8`
)

var defaultFormat = responseFormat{contentType: contentTypeJSON}

// negotiate picks the response casing from the Accept header the way
// Jellyfin 12.1.0 (ASP.NET) does, verified against the recon server:
//   - media ranges are tried in descending q order (ties keep header order);
//   - the first JSON-capable range (application/json, application/*, */*) wins;
//   - camelCase only if that range carries profile=CamelCase, quoted or not;
//   - everything else, including no Accept header, is PascalCase.
//
// So Jellyfin Android's "application/json,application/json; profile=CamelCase,…"
// gets PascalCase, because the plain range comes first.
func negotiate(accept []string) responseFormat {
	type rng struct {
		mediaType string
		profile   string
		q         float64
	}
	var ranges []rng
	for _, header := range accept {
		for _, part := range strings.Split(header, ",") {
			mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			q := 1.0
			if v, ok := params["q"]; ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					q = f
				}
			}
			if q <= 0 {
				continue
			}
			ranges = append(ranges, rng{mediaType: mt, profile: params["profile"], q: q})
		}
	}
	sort.SliceStable(ranges, func(i, j int) bool { return ranges[i].q > ranges[j].q })
	for _, r := range ranges {
		switch r.mediaType {
		case "application/json", "application/*", "*/*":
		default:
			continue
		}
		switch {
		case strings.EqualFold(r.profile, "CamelCase"):
			return responseFormat{camel: true, contentType: contentTypeCamelJSON}
		case strings.EqualFold(r.profile, "PascalCase"):
			return responseFormat{contentType: contentTypePascalJSON}
		default:
			return defaultFormat
		}
	}
	return defaultFormat
}
