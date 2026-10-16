package sandboxsnapshot

import "strings"

// boundedQuantityExponent runs after the wire budget check and before the
// quantity parser. A short exponent must not trigger unbounded decimal scale
// expansion or overflow the parser's narrower exponent representation.
func boundedQuantityExponent(value string) bool {
	index := strings.IndexAny(value, "eE")
	if index < 0 {
		return true
	}
	suffix := value[index+1:]
	// E and Ei are SI/binary suffixes rather than decimal exponents. The
	// quantity parser remains responsible for validating the full grammar.
	if suffix == "" || suffix == "i" {
		return true
	}
	if suffix[0] == '+' || suffix[0] == '-' {
		suffix = suffix[1:]
	}
	if suffix == "" {
		return false
	}
	magnitude := 0
	for _, digit := range []byte(suffix) {
		if digit < '0' || digit > '9' {
			return false
		}
		next := int(digit - '0')
		if magnitude > (MaxBytes-next)/10 {
			return false
		}
		magnitude = magnitude*10 + next
	}
	return true
}
