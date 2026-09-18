// Package redact masks credentials before anything reaches a log, an API
// response or a notification.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// URL returns a display-safe form of a URL. Credential-bearing webhook paths
// keep their prefix and mask the token; credentials in userinfo are masked.
func URL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Token(raw)
	}
	out := u.Scheme + "://" + u.Host
	if u.User != nil {
		out = u.Scheme + "://" + Token(u.User.Username()) + "@***@" + u.Host
	}
	switch {
	case strings.Contains(u.Path, "/hook/"):
		i := strings.LastIndex(u.Path, "/hook/") + len("/hook/")
		return out + u.Path[:i] + Token(strings.TrimPrefix(u.Path, u.Path[:i]))
	default:
		return out + u.Path
	}
}

// Token shortens a secret into a recognizable but unusable fingerprint.
func Token(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + "…" + strings.Repeat("*", 4) + string(r[len(r)-2:])
}

// Query keeps parameter names but replaces every value.
func Query(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	var parts []string
	for k := range u.Query() {
		parts = append(parts, k+"=***")
	}
	u.RawQuery = strings.Join(parts, "&")
	return u.String()
}

var secretAssign = regexp.MustCompile(`(?i)((?:authorization|api[_-]?key|secret|password|token|webhook)\s*[=:]\s*)(["']?)((?:bearer|basic|digest|token)\s+)?([^\s,;'"]{4,})`)

// Text masks `key: value` style secrets inside free text such as an error body
// echoed back from an upstream. Auth schemes and quoting are preserved so the
// message stays readable.
func Text(s string) string {
	return secretAssign.ReplaceAllStringFunc(s, func(m string) string {
		parts := secretAssign.FindStringSubmatch(m)
		if len(parts) < 5 {
			return m
		}
		return parts[1] + parts[2] + parts[3] + Token(parts[4])
	})
}
