package store

import (
	"fmt"
	"sort"
	"strings"
)

// Request tags are the caller's own labels for a request, sent as
// X-Proxy-Tags: feature=search,env=prod. They are stored with its usage
// event and usage can be grouped and filtered by them.
const (
	MaxTags        = 10
	maxTagKeyLen   = 40
	maxTagValueLen = 100
)

// ParseTags reads a tags header: comma-separated key=value pairs. Keys are
// lowercase letters, digits, '_', '-' and '.'; values may also hold
// uppercase letters, ':', '/', '@' and '+'. Neither may hold ',' or '=', so
// the canonical form (see FormatTags) can be split back apart.
func ParseTags(header string) (map[string]string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, nil
	}
	tags := map[string]string{}
	for _, pair := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case !ok || !validTagKey(k):
			return nil, fmt.Errorf("tag %q: keys are 1-%d characters of a-z, 0-9, '_', '-' and '.', followed by =value", pair, maxTagKeyLen)
		case !validTagValue(v):
			return nil, fmt.Errorf("tag %q: values are 1-%d characters of letters, digits and _ - . : / @ +", pair, maxTagValueLen)
		}
		if _, dup := tags[k]; dup {
			return nil, fmt.Errorf("tag %q is given twice", k)
		}
		tags[k] = v
	}
	if len(tags) > MaxTags {
		return nil, fmt.Errorf("at most %d tags per request", MaxTags)
	}
	return tags, nil
}

// FormatTags writes tags as key=value pairs sorted by key, joined by commas.
func FormatTags(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k + "=" + tags[k])
	}
	return b.String()
}

func validTagKey(k string) bool {
	if k == "" || len(k) > maxTagKeyLen {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func validTagValue(v string) bool {
	if v == "" || len(v) > maxTagValueLen {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-.:/@+", c)) {
			return false
		}
	}
	return true
}
