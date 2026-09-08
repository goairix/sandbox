// Package imageref validates container image references used by sandbox
// releases.
package imageref

import (
	"regexp"
	"strings"

	"github.com/distribution/reference"
)

var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?$`)

// IsRelease reports whether image is pinned by a lowercase sha256 digest or
// names an explicit semantic release version. Mutable and ad-hoc tags are not
// accepted.
func IsRelease(image string) bool {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false
	}
	if digested, ok := named.(reference.Digested); ok {
		digest := digested.Digest()
		encoded := digest.Encoded()
		return digest.Algorithm() == "sha256" && len(encoded) == 64 &&
			encoded == strings.ToLower(encoded) && isLowerHex(encoded)
	}
	tagged, ok := named.(reference.Tagged)
	return ok && releaseTag.MatchString(tagged.Tag())
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
