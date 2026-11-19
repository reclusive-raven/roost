package redact

import (
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	in := "mail a.person@example.test, key ghp_FAKEFAKEFAKEFAKEFAKE, token=abc123, card 4111 1111 1111 1111, " +
		"https://user:pw@host.example.test/p?secret=1, Bearer FAKE.TOKEN, call +1 555 010 9999, run 20261012-150405"
	got := Text(in)
	for _, leak := range []string{"a.person@", "ghp_", "abc123", "4111 1111", "user:pw", "secret=1", "FAKE.TOKEN", "555 010"} {
		if strings.Contains(got, leak) {
			t.Errorf("kept %q in %q", leak, got)
		}
	}
	for _, keep := range []string{"••••1111", "https://host.example.test/p", "20261012-150405"} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost %q in %q", keep, got)
		}
	}
}
