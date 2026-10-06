package etcd

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestNamespace(t *testing.T) {
	n, err := NewNamespace("/sandbox/v1", "prod", "cell-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Root(); got != "/sandbox/v1/prod/cell-a/" {
		t.Fatalf("Root = %q", got)
	}
	key, err := n.Key("meta", "identity")
	if err != nil || key != n.Root()+"meta/identity" {
		t.Fatalf("Key = %q, %v", key, err)
	}
	for _, prefix := range []string{"", "/", "relative", "/a/", "//a", "/a//b", "/a/./b", "/a/../b", "/a\n", "/a b", "/a%2fb"} {
		if _, err := NewNamespace(prefix, "scope", "cell"); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("prefix %q accepted: %v", prefix, err)
		}
	}
	invalid := []string{"", ".", "..", "a/b", "a\\b", "a b", "a\n", "a\x00", "%2f", "中文"}
	for _, s := range invalid {
		if _, err := NewNamespace("/valid", s, "cell"); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("scope %q accepted", s)
		}
		if _, err := NewNamespace("/valid", "scope", s); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("cell %q accepted", s)
		}
		if _, err := n.Key(s); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("segment %q accepted", s)
		}
	}
	if _, err := n.Key(); !errors.Is(err, ErrInvalidConfiguration) {
		t.Errorf("empty key accepted")
	}
	if _, err := (Namespace{}).Key("meta"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Errorf("zero namespace accepted")
	}
	if _, err := NewNamespace("/A_1/a.b-c", "scope_1", "cell.2"); err != nil {
		t.Fatal(err)
	}
}

func TestPartition(t *testing.T) {
	for _, identity := range [][]byte{nil, {}, []byte("sandbox-1"), []byte("sandbox-2"), {0, 255}} {
		want := sha256.Sum256(identity)
		if got := Partition(identity); got != want[0] {
			t.Fatalf("Partition(%q) = %d, want %d", identity, got, want[0])
		}
	}
}

func TestNamespaceLengthLimits(t *testing.T) {
	for name, values := range map[string][3]string{
		"root":  {"/" + strings.Repeat("p", 507), "s", "c"},
		"scope": {"/prefix", strings.Repeat("s", 129), "c"},
		"cell":  {"/prefix", "s", strings.Repeat("c", 129)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewNamespace(values[0], values[1], values[2]); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("oversized namespace accepted: %v", err)
			}
		})
	}
	n, err := NewNamespace("/"+strings.Repeat("p", 506), "s", "c")
	if err != nil {
		t.Fatalf("512-byte root rejected: %v", err)
	}
	if len(n.Root()) != 512 {
		t.Fatalf("test root has %d bytes", len(n.Root()))
	}
	if _, err := n.Key(strings.Repeat("k", 512)); err != nil {
		t.Fatalf("1024-byte key rejected: %v", err)
	}
	if _, err := n.Key(strings.Repeat("k", 513)); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("oversized key accepted: %v", err)
	}
}
