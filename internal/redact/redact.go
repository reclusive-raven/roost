// Package redact removes recognisable credentials and personal identifiers
// from text before roost stores it in the search index. It is pattern
// matching, not a guarantee.
package redact

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Hidden stands in for a value removed whole. Card-length digit runs are
// masked to their last four instead.
const Hidden = "[redacted]"

var (
	urls        = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"']+`)
	credentials = regexp.MustCompile(`(?i)(["']?(?:[a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|authorization|cookie|private[_-]?key|client[_-]?secret))["']?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`)
	auth        = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[a-z0-9+/_.=-]+`)
	tokens      = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+|sk-[A-Za-z0-9_-]+|(?:AC|SK)[a-fA-F0-9]{32}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|npg_[A-Za-z0-9]+|xox[abprs]-[A-Za-z0-9-]+|glpat-[A-Za-z0-9_-]+|hf_[A-Za-z0-9]{30,})\b`)
	email       = regexp.MustCompile(`(?i)[a-z0-9.!#$%&'*+/=?^_\x60{|}~-]+@[a-z0-9.-]+\.[a-z]{2,}`)
	phone       = regexp.MustCompile(`\+[1-9][0-9 ()-]{7,}[0-9]`)
	// stamp is a compact date and time (20261012-150405); its fourteen digits
	// are not a card number.
	stamp  = regexp.MustCompile(`^(?:19|20)[0-9]{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12][0-9]|3[01])-(?:[01][0-9]|2[0-3])[0-5][0-9][0-5][0-9]$`)
	digits = regexp.MustCompile(`[0-9]+(?:[\p{Zs}\p{Cc}\p{Cf}-]+[0-9]+)*`)
)

// Text redacts recognisable credentials, URL secrets, email addresses, phone
// numbers and card-length digit runs.
func Text(s string) string {
	s = urls.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return Hidden
		}
		u.User = nil
		u.Path = Text(u.Path)
		u.RawPath = ""
		u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
		u.ForceQuery = false
		return u.String()
	})
	s = auth.ReplaceAllString(s, "${1} "+Hidden)
	s = credentials.ReplaceAllString(s, `${1}"`+Hidden+`"`)
	s = tokens.ReplaceAllString(s, Hidden)
	s = email.ReplaceAllString(s, Hidden)
	s = phone.ReplaceAllString(s, Hidden)
	return digits.ReplaceAllStringFunc(s, func(run string) string {
		if stamp.MatchString(run) {
			return run
		}
		return maskDigits(run)
	})
}

// maskDigits masks a card-length run to its last four. A longer run of short
// groups is ambiguous, so it goes whole rather than expose an inner group.
func maskDigits(run string) string {
	groups := strings.FieldsFunc(run, func(c rune) bool {
		return unicode.IsSpace(c) || unicode.IsControl(c) || unicode.In(c, unicode.Cf) || c == '-'
	})
	var joined strings.Builder
	standalone := true
	for _, group := range groups {
		joined.WriteString(group)
		standalone = standalone && len(group) >= 13 && len(group) <= 19
	}
	switch {
	case joined.Len() < 13:
		return run
	case joined.Len() <= 19:
		n := joined.String()
		return "••••" + n[len(n)-4:]
	case standalone:
		for i, group := range groups {
			groups[i] = "••••" + group[len(group)-4:]
		}
		return strings.Join(groups, " ")
	default:
		return Hidden
	}
}
