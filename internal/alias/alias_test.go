package alias

import (
	"strings"
	"testing"
	"time"
)

func TestCheck(t *testing.T) {
	now := time.Unix(1_710_000_000, 0)
	const tok = "healthcheck:ok"

	cases := []struct {
		name    string
		alias   string
		maxAge  int
		wantOK  bool
		reason  string // substring; empty = don't check
	}{
		{"ok bare", "healthcheck:ok", 300, true, ""},
		{"ok with date fresh", "healthcheck:ok;date:1709999900", 300, true, ""},
		{"ok date exactly max", "healthcheck:ok;date:1709999700", 300, true, ""},
		{"stale date", "healthcheck:ok;date:1709999699", 300, false, "stale"},
		{"missing token", "date:1710000000", 300, false, "missing token"},
		{"empty", "", 300, false, "missing token"},
		{"fail token", "healthcheck:fail", 300, false, "missing token"},
		{"no date always fresh", "healthcheck:ok", 300, true, ""},
		{"junk ignored", "healthcheck:ok;signed:DEADBEEF;foo:bar", 300, true, ""},
		{"spaces", " healthcheck:ok ; date:1709999900 ", 300, true, ""},
		{"invalid date", "healthcheck:ok;date:notanumber", 300, false, "invalid date"},
		{"maxAge 0 never stale", "healthcheck:ok;date:1", 0, true, ""},
		{"token with other order", "date:1709999900;healthcheck:ok;signed:x", 300, true, ""},
		{"cleared alias", "", 300, false, "missing"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Check(tc.alias, tok, tc.maxAge, now)
			if r.OK != tc.wantOK {
				t.Fatalf("OK=%v want %v reason=%q", r.OK, tc.wantOK, r.Reason)
			}
			if tc.reason != "" && !contains(r.Reason, tc.reason) {
				t.Fatalf("reason %q does not contain %q", r.Reason, tc.reason)
			}
		})
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
