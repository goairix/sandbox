package storage

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidWorkspacePrefix indicates that a workspace prefix component is
// not a canonical relative object-key path.
var ErrInvalidWorkspacePrefix = errors.New("invalid workspace prefix")

// BuildWorkspacePrefix validates and joins the canonical storage sub-path and
// workspace path. The returned object prefix has exactly one trailing slash.
func BuildWorkspacePrefix(subPath, workspacePath string) (string, error) {
	if err := validateCanonicalRelativePrefix(subPath, true); err != nil {
		return "", fmt.Errorf("sub_path: %w", err)
	}
	if err := validateCanonicalRelativePrefix(workspacePath, false); err != nil {
		return "", fmt.Errorf("workspace_path: %w", err)
	}
	if subPath == "" {
		return workspacePath + "/", nil
	}
	return subPath + "/" + workspacePath + "/", nil
}

func validateCanonicalRelativePrefix(value string, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if value == "" || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return ErrInvalidWorkspacePrefix
	}

	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return ErrInvalidWorkspacePrefix
		}
	}

	segments := strings.Split(value, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return ErrInvalidWorkspacePrefix
		}
	}
	if segments[0] == ".sandbox-system" {
		return ErrInvalidWorkspacePrefix
	}

	return nil
}
