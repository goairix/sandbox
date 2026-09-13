package redisbootstrap

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// QuoteConfigValue renders exactly one Redis double-quoted configuration value.
// It is not shell quoting; all Unicode controls and malformed UTF-8 are rejected.
func QuoteConfigValue(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("configuration value is invalid UTF-8")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", errors.New("configuration value contains a control character")
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`, nil
}
