// Package execcode shares the v1 language-to-shell-command mapping.
package execcode

import (
	"fmt"
	"strings"
)

func BuildCommand(language, code string) (string, error) {
	quote := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
	}
	switch language {
	case "python":
		return "python3 -c " + quote(code), nil
	case "nodejs":
		return "node -e " + quote(code), nil
	case "bash":
		return code, nil
	default:
		return "", fmt.Errorf("unsupported language: %s", language)
	}
}
