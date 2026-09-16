package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseFileCount(t *testing.T) {
	for _, text := range []string{"", "-1", "1 suffix", "999999999999999999999"} {
		_, err := ParseFileCount(text)
		require.EqualError(t, err, "invalid file count")
	}
	for _, tc := range []struct {
		text string
		want int
	}{{"0", 0}, {" 42\n", 42}} {
		count, err := ParseFileCount(tc.text)
		require.NoError(t, err)
		require.Equal(t, tc.want, count)
	}
}

func TestParseFileMetadata(t *testing.T) {
	for _, tc := range []struct{ size, modified string }{
		{"-1", "0"}, {"1 suffix", "0"}, {"999999999999999999999", "0"},
		{"0", "NaN"}, {"0", "+Inf"}, {"0", "-Inf"}, {"0", "1 suffix"},
		{"0", "9223372036854775808"}, {"0", "-9223372036854775809"},
	} {
		_, _, err := ParseFileMetadata(tc.size, tc.modified)
		require.EqualError(t, err, "invalid file metadata")
	}
	size, modified, err := ParseFileMetadata("42", "1700000000.25")
	require.NoError(t, err)
	require.Equal(t, int64(42), size)
	require.Equal(t, time.Unix(1700000000, 250000000), modified)
	_, modified, err = ParseFileMetadata("0", "-1.25")
	require.NoError(t, err)
	require.Equal(t, time.Unix(-1, -250000000), modified)
}
