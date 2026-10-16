package deployment

import (
	"bytes"
	"strings"
	"testing"
)

func TestManifestStrictJSON(t *testing.T) {
	for name, wire := range map[string][]byte{
		"unknown":              []byte(`{"version":1,"secret":"sentinel"}`),
		"duplicate":            []byte(`{"version":1,"version":1}`),
		"nested duplicate":     []byte(`{"version":1,"etcd":{"identity":{"cell":"a","cell":"b"}}}`),
		"case alias":           []byte(`{"Version":1}`),
		"unicode root alias":   []byte(`{"ver\u017fion":1}`),
		"unicode nested alias": []byte(`{"version":1,"resources":{"di\u017fk":"1Mi"}}`),
		"trailing":             []byte(`{"version":1} {}`),
		"invalid UTF8":         append([]byte(`{"version":1,"image":"`), 255, '"', '}'),
		"wrong version":        []byte(`{"version":2}`),
		"oversize":             bytes.Repeat([]byte(" "), 65537),
		"array":                []byte(`[]`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseManifest(wire); err == nil || strings.Contains(err.Error(), "sentinel") {
				t.Fatalf("unsafe manifest result: %v", err)
			}
		})
	}
	if _, err := parseManifest([]byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
}
