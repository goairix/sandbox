package main

import "testing"

// Default disk EmptyDir and memory tmpfs permissions must reach the protected
// FD tightening step, while special bits and non-directory types must not.
func TestEnrolledProvisionFreshControlMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode uint32
		want bool
	}{
		{"private", 0040700, true},
		{"legacy", 0040755, true},
		{"empty-dir", 0040777, true},
		{"memory-empty-dir", 0041777, true},
		{"group-writable", 0040770, false},
		{"sticky-private", 0041700, false},
		{"setgid", 0042777, false},
		{"setuid", 0044777, false},
		{"all-special", 0047777, false},
		{"regular", 0100777, false},
		{"symlink", 0120777, false},
		{"socket", 0140777, false},
		{"missing-type", 01777, false},
		{"unknown-high-bit", 0x10000 | 0040700, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := freshControlMode(tc.mode); got != tc.want {
				t.Fatalf("initial mode %#o admission = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}
