// Package alias parses iface IFLA_IFALIAS healthcheck strings.
package alias

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Result is the outcome of parsing an alias.
type Result struct {
	OK     bool
	Reason string // empty when OK
}

// Check parses alias and applies health rules.
// requireToken is matched as a full field (e.g. "healthcheck:ok").
// maxAgeSeconds <= 0 means date is never considered stale.
func Check(alias, requireToken string, maxAgeSeconds int, now time.Time) Result {
	if requireToken == "" {
		requireToken = "healthcheck:ok"
	}
	tokenOK := false
	var date int64
	hasDate := false

	for _, f := range strings.Split(alias, ";") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if f == requireToken {
			tokenOK = true
			continue
		}
		key, val, ok := strings.Cut(f, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "date":
			n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			if err != nil {
				return Result{Reason: "invalid date"}
			}
			date, hasDate = n, true
		}
		// signed + unknown keys ignored
	}

	if !tokenOK {
		return Result{Reason: "missing token"}
	}
	if hasDate && maxAgeSeconds > 0 {
		if age := now.Unix() - date; age > int64(maxAgeSeconds) {
			return Result{Reason: fmt.Sprintf("stale date (age %ds > %ds)", age, maxAgeSeconds)}
		}
	}
	return Result{OK: true}
}
