//go:build linux

package launcher

import "testing"

func TestPID1QuiescenceSealParsing(t *testing.T) {
	for _, raw := range []string{"0::/\n", "0::/docker/abc\n"} {
		if _, err := canonicalCgroup(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"0::/a/../b\n", "0::/a\n0::/b\n", "1:cpu:/\n", "0::/", "0::/a\\b\n"} {
		if _, err := canonicalCgroup(raw); err == nil {
			t.Fatalf("accepted ambiguous membership %q", raw)
		}
	}
	mount := "1 2 0:3 / /proc rw,nosuid,nodev - proc proc rw\n"
	if _, err := mountIdentity(mount, "/proc", "proc"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{mount + mount, "1 2 0:3 / /proc rw - tmpfs tmpfs rw\n", "malformed\n"} {
		if _, err := mountIdentity(raw, "/proc", "proc"); err == nil {
			t.Fatalf("accepted ambiguous mount %q", raw)
		}
	}
}
