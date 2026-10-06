package etcd

import (
	"errors"
	"strings"
	"testing"
)

func TestDomainWorkspaceIdentityStableHash(t *testing.T) {
	w, err := NewWorkspaceIdentity("minio", "store", "bucket", "work/")
	if err != nil {
		t.Fatal(err)
	}
	if w.Hash() != "1bab2aa6b5393553247aaca84415eed0c40a14e1436b8b152ea171249cd920dc" || w.Partition() != 0x1b {
		t.Fatalf("wrong hash/partition %q %x", w.Hash(), w.Partition())
	}
	for _, p := range [][4]string{{"obs", "store", "bucket", "work/"}, {"minio", "other", "bucket", "work/"}, {"minio", "store", "other", "work/"}, {"minio", "store", "bucket", "other/"}} {
		other, e := NewWorkspaceIdentity(p[0], p[1], p[2], p[3])
		if e != nil || other.Hash() == w.Hash() {
			t.Errorf("identity not isolated: %v %v", p, e)
		}
	}
	a, _ := NewWorkspaceIdentity("minio", "store", "ab", "c/")
	b, _ := NewWorkspaceIdentity("minio", "store", "a", "bc/")
	if a.Hash() == b.Hash() {
		t.Error("length framing collision")
	}
}
func TestDomainWorkspaceIdentityRejectsInvalidInput(t *testing.T) {
	valid := [4]string{"minio", "store", "bucket", "work/"}
	for _, tc := range []struct {
		field int
		value string
	}{{0, ""}, {1, ""}, {2, ""}, {0, strings.Repeat("a", 129)}, {1, strings.Repeat("a", 129)}, {2, strings.Repeat("a", 257)}, {3, strings.Repeat("a", 1024) + "/"}, {3, "work"}, {3, "work//"}, {3, "../work/"}, {3, "/work/"}, {3, "a/../work/"}, {3, ".sandbox-system/x/"}, {3, "a//b/"}, {3, "a\x00/"}, {3, "\xff/"}, {1, "\x7f"}, {0, "a\n"}, {2, "a\u0085"}} {
		p := valid
		p[tc.field] = tc.value
		_, err := NewWorkspaceIdentity(p[0], p[1], p[2], p[3])
		if !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("accepted field %d invalid value: %v", tc.field, err)
		}
	}
	for _, p := range [][4]string{{strings.Repeat("a", 128), strings.Repeat("b", 128), strings.Repeat("c", 256), strings.Repeat("d", 1023) + "/"}, {"服务", "仓库", "桶", "目录/"}} {
		if _, err := NewWorkspaceIdentity(p[0], p[1], p[2], p[3]); err != nil {
			t.Errorf("valid input: %v", err)
		}
	}
}
func TestDomainRequestKeyHash(t *testing.T) {
	h, err := requestKeyHash("user", "key")
	if err != nil || h != "8e9dcf9da50b4e66e797154b1e0cb79b2fa908091282891462faf26d6b69bcc9" {
		t.Fatalf("hash %q: %v", h, err)
	}
	a, _ := requestKeyHash("ab", "c")
	b, _ := requestKeyHash("a", "bc")
	c, _ := requestKeyHash("other", "key")
	if a == b || h == c {
		t.Error("principal or framing ignored")
	}
	for _, p := range [][2]string{{"", "k"}, {"p", ""}, {"p", "\xff"}, {"\x00", "k"}, {strings.Repeat("a", 129), "k"}, {"p", strings.Repeat("k", 257)}} {
		if _, err := requestKeyHash(p[0], p[1]); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("accepted invalid request: %v", err)
		}
	}
}
