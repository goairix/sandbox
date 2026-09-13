package redisbootstrap

import (
	"testing"
	"unicode"
)

func TestQuoteConfigValue(t *testing.T) {
	for _, tt := range []struct{ input, want string }{{"", `""`}, {`a b"c\d`, `"a b\"c\\d"`}, {"正常 密码", `"正常 密码"`}} {
		got, err := QuoteConfigValue(tt.input)
		if err != nil || got != tt.want {
			t.Fatalf("quote %q = %q,%v", tt.input, got, err)
		}
	}
	for r := rune(0); r <= 0x9f; r++ {
		if unicode.IsControl(r) {
			if _, err := QuoteConfigValue(string(r)); err == nil {
				t.Fatalf("accepted control U+%04X", r)
			}
		}
	}
	if _, err := QuoteConfigValue(string([]byte{0xff})); err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
}
