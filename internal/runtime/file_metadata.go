package runtime

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseFileCount rejects partial, negative and overflowing command output.
// Errors deliberately omit tenant-controlled output.
func ParseFileCount(text string) (int, error) {
	count, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || count < 0 {
		return 0, errors.New("invalid file count")
	}
	return count, nil
}

// ParseFileMetadata parses find's size and fractional Unix timestamp fields.
func ParseFileMetadata(sizeText, modifiedText string) (int64, time.Time, error) {
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size < 0 {
		return 0, time.Time{}, errors.New("invalid file metadata")
	}
	modified, err := strconv.ParseFloat(modifiedText, 64)
	// Float64 rounds values at the int64 boundaries, so reject both boundary
	// values before conversion rather than accepting an overflowing timestamp.
	if err != nil || math.IsNaN(modified) || math.IsInf(modified, 0) || modified <= -0x1p63 || modified >= 0x1p63 {
		return 0, time.Time{}, errors.New("invalid file metadata")
	}
	seconds, fraction := math.Modf(modified)
	return size, time.Unix(int64(seconds), int64(fraction*1e9)), nil
}
