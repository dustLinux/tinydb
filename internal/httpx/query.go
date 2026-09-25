package httpx

import "strings"

// Values is a parsed URL query (replaces url.Values: same Get/range API,
// without importing package net).
type Values map[string][]string

// Get returns the first value for key, or "".
func (v Values) Get(key string) string {
	if vs := v[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// Set replaces the value for key.
func (v Values) Set(key, value string) { v[key] = []string{value} }

// parseQuery parses "a=1&b=2" with percent-decoding ('+' = space).
// Malformed pairs are skipped rather than failing the request.
func parseQuery(s string) Values {
	v := Values{}
	for s != "" {
		var pair string
		if i := strings.IndexByte(s, '&'); i >= 0 {
			pair, s = s[:i], s[i+1:]
		} else {
			pair, s = s, ""
		}
		if pair == "" {
			continue
		}
		var k, val string
		if i := strings.IndexByte(pair, '='); i >= 0 {
			k, val = pair[:i], pair[i+1:]
		} else {
			k, val = pair, ""
		}
		k = queryUnescape(k)
		if k == "" {
			continue
		}
		v[k] = append(v[k], queryUnescape(val))
	}
	return v
}

// queryUnescape decodes %XX escapes; '+' becomes a space. On bad input the
// original text is kept (lenient, like browsers expect for filters).
func queryUnescape(s string) string {
	if !strings.ContainsRune(s, '%') && !strings.ContainsRune(s, '+') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(s):
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// pathUnescape decodes %XX escapes in a path segment ('+' stays literal).
func pathUnescape(s string) (string, error) {
	return queryUnescapeKeepPlus(s), nil
}

func queryUnescapeKeepPlus(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
