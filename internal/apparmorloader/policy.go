// Package apparmorloader manages one immutable, content-addressed AppArmor profile.
package apparmorloader

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

const ProfilePlaceholder = "__SANDBOX_PROFILE_NAME__"

var (
	digestPattern              = regexp.MustCompile(`^[0-9a-f]{64}$`)
	declarationPattern         = regexp.MustCompile(`(?m)^profile ([a-z0-9-]+) flags=\(attach_disconnected,mediate_deleted\) \{$`)
	includePattern             = regexp.MustCompile(`(?m)^\s*#?\s*include\b`)
	abiPattern                 = regexp.MustCompile(`(?m)(^|,)\s*abi\b`)
	childPattern               = regexp.MustCompile(`\bprofile\s+|(^|\s)\^[a-zA-Z]|\bchange_profile\b|\bcomplain\b`)
	pathnameAlternationPattern = regexp.MustCompile(`\{[^{}\s]*,[^{}\s]*\}`)
	permissionTokenPattern     = regexp.MustCompile(`^[rwalkmixupUcPC]+$`)
)

// CanonicalPolicy is shared with Chart hashing: CRLF to LF, trim, final LF.
func CanonicalPolicy(policy []byte) []byte {
	return []byte(strings.TrimSpace(strings.ReplaceAll(string(policy), "\r\n", "\n")) + "\n")
}

// ValidatePolicy verifies the rendered version against the self-contained template.
// Restricting declarations also prevents loading unrelated profiles from one input.
func ValidatePolicy(policy []byte, name, digest string) error {
	if !digestPattern.MatchString(digest) || name != "sandbox-fuse-"+digest {
		return errors.New("invalid content-addressed profile identity")
	}
	canonical := string(CanonicalPolicy(policy))
	if strings.Contains(canonical, ProfilePlaceholder) || strings.Count(canonical, name) != 1 {
		return errors.New("profile name must occur exactly once")
	}
	if includePattern.MatchString(canonical) {
		return errors.New("external policy includes are forbidden")
	}
	matches := declarationPattern.FindAllStringSubmatchIndex(canonical, -1)
	if len(matches) != 1 || canonical[matches[0][2]:matches[0][3]] != name {
		return errors.New("expected one exact enforce profile declaration")
	}
	body := canonical[:matches[0][0]] + canonical[matches[0][1]:]
	rules, err := policyCode(body)
	if err != nil {
		return err
	}
	// ABI directives resolve external feature metadata just like includes resolve
	// external rules. Check masked code so quoted paths and comments stay inert.
	if abiPattern.MatchString(rules) {
		return errors.New("external policy ABI directives are forbidden")
	}
	if childPattern.MatchString(rules) {
		return errors.New("child profiles, transitions and complain policies are forbidden")
	}
	if err := validateInheritanceExecution(rules); err != nil {
		return err
	}
	// No additional block may occur; braces inside pathname alternations remain valid.
	depth := 1
	for _, line := range strings.Split(rules, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "}" {
			depth--
			if depth < 0 {
				return errors.New("multiple policy blocks")
			}
			continue
		}
		withoutAlternations := pathnameAlternationPattern.ReplaceAllString(line, "")
		if depth != 1 || strings.ContainsAny(withoutAlternations, "{}") {
			return errors.New("unexpected policy block")
		}
	}
	if depth != 0 {
		return errors.New("unterminated policy block")
	}
	template := strings.Replace(canonical, name, ProfilePlaceholder, 1)
	sum := sha256.Sum256([]byte(template))
	if hex.EncodeToString(sum[:]) != digest {
		return errors.New("profile content digest mismatch")
	}
	return nil
}

// Validate permission tokens in either leading or trailing file-rule syntax.
// Only ix may grant execution; even profile/fallback transitions are rejected.
// Policy paths and comments have already been masked by policyCode.
func validateInheritanceExecution(rules string) error {
	tokens := strings.Fields(strings.NewReplacer(",", " ", "(", " ", ")", " ").Replace(rules))
	for _, token := range tokens {
		if !permissionTokenPattern.MatchString(token) || !strings.ContainsRune(token, 'x') {
			continue
		}
		remaining := strings.Replace(token, "ix", "", 1)
		for _, permission := range remaining {
			if !strings.ContainsRune("rwalkm", permission) {
				return errors.New("only inherited ix execution permissions are allowed")
			}
		}
	}
	return nil
}

// policyCode removes comments and quoted pathname contents, retaining structural
// tokens after the closing quote. Backslash escapes cannot close a quoted path.
// Includes are checked before this scan because #include is not a comment.
func policyCode(body string) (string, error) {
	var code strings.Builder
	quoted, escaped, comment := false, false, false
	for _, r := range body {
		if r == '\n' {
			if quoted || escaped {
				return "", errors.New("unterminated quoted pathname or escape")
			}
			comment = false
			code.WriteRune(r)
			continue
		}
		if comment {
			continue
		}
		if escaped {
			escaped = false
			if !quoted {
				code.WriteByte('_')
			}
			continue
		}
		if r == '\\' {
			escaped = true
			if !quoted {
				code.WriteByte('_')
			}
			continue
		}
		if r == '"' {
			quoted = !quoted
			code.WriteByte(' ')
			continue
		}
		if quoted {
			continue
		}
		if r == '#' {
			comment = true
			continue
		}
		code.WriteRune(r)
	}
	if quoted || escaped {
		return "", errors.New("unterminated quoted pathname or escape")
	}
	return code.String(), nil
}
