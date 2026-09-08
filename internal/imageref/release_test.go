package imageref

import (
	"strings"
	"testing"
)

func TestIsRelease(t *testing.T) {
	digest := "registry.example.com/sandbox@sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name  string
		image string
		want  bool
	}{
		{name: "stable version", image: "registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.2.12", want: true},
		{name: "prerelease version", image: "registry.i.huaxisy.com/library/ai-infra/sandbox-api:v0.2.12-rc.1", want: true},
		{name: "legacy digest", image: digest, want: true},
		{name: "latest", image: "registry.example.com/sandbox:latest", want: false},
		{name: "untagged", image: "registry.example.com/sandbox", want: false},
		{name: "architecture tag", image: "registry.example.com/sandbox:arm64", want: false},
		{name: "missing v prefix", image: "registry.example.com/sandbox:0.2.12", want: false},
		{name: "leading zero", image: "registry.example.com/sandbox:v01.2.3", want: false},
		{name: "uppercase digest", image: "registry.example.com/sandbox@sha256:" + strings.Repeat("A", 64), want: false},
		{name: "short digest", image: "registry.example.com/sandbox@sha256:abc", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRelease(tt.image); got != tt.want {
				t.Fatalf("IsRelease(%q) = %v, want %v", tt.image, got, tt.want)
			}
		})
	}
}
