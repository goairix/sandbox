package filev2

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFilesV2TextReadLines(t *testing.T) {
	const content = "α\r\nβ\nlast"
	for _, tc := range []struct {
		start, count int
		want         string
	}{
		{1, 1, "α\r\n"},
		{2, 0, "β\nlast"},
		{3, 1, "last"},
		{4, 0, ""},
	} {
		got, err := readLines(context.Background(), strings.NewReader(content), tc.start, tc.count)
		if err != nil || got != tc.want {
			t.Fatalf("readLines(%d,%d) = %q, %v; want %q", tc.start, tc.count, got, err, tc.want)
		}
	}
	if _, err := readLines(context.Background(), strings.NewReader(content), 0, 1); err == nil {
		t.Fatal("accepted zero start line")
	}
	if _, err := readLines(context.Background(), strings.NewReader(strings.Repeat("x", (1<<20)+1)), 1, 0); !errors.Is(err, ErrTextLimit) {
		t.Fatalf("unbounded text result returned %v", err)
	}
}

func TestFilesV2TextEdit(t *testing.T) {
	result, count, err := editText([]byte("α one\r\nβ two\n"), "one", "ONE", false)
	if err != nil || count != 1 || string(result) != "α ONE\r\nβ two\n" {
		t.Fatalf("single edit = %q, %d, %v", result, count, err)
	}
	if _, _, err := editText([]byte("x x"), "x", "y", false); !errors.Is(err, ErrEditConflict) {
		t.Fatalf("ambiguous edit returned %v", err)
	}
	result, count, err = editText([]byte("x x"), "x", "y", true)
	if err != nil || count != 2 || string(result) != "y y" {
		t.Fatalf("replace all = %q, %d, %v", result, count, err)
	}
	for _, old := range []string{"", "missing"} {
		if _, _, err := editText([]byte("x"), old, "y", false); err == nil {
			t.Fatalf("accepted old text %q", old)
		}
	}
	if _, _, err := editText([]byte("x"), "x", "x", true); err == nil {
		t.Fatal("accepted identical replacement")
	}
	if _, _, err := editText([]byte("x"), "x", strings.Repeat("y", (8<<20)+1), false); !errors.Is(err, ErrTextLimit) {
		t.Fatalf("oversized replacement returned %v", err)
	}
}
