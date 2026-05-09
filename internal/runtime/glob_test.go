package runtime

import "testing"

func TestGlobToFindArgs(t *testing.T) {
	tests := []struct {
		pattern       string
		wantArgs      string
		wantMaxDepth1 bool
	}{
		{
			pattern:       "**/*.txt",
			wantArgs:      "-name '*.txt'",
			wantMaxDepth1: false,
		},
		{
			pattern:       "*.txt",
			wantArgs:      "-name '*.txt'",
			wantMaxDepth1: true,
		},
		{
			pattern:       "**/*.{txt,md}",
			wantArgs:      "\\( -name '*.txt' -o -name '*.md' \\)",
			wantMaxDepth1: false,
		},
		{
			pattern:       "**/*医疗*.txt",
			wantArgs:      "-name '*医疗*.txt'",
			wantMaxDepth1: false,
		},
		{
			pattern:       "*.{go,mod}",
			wantArgs:      "\\( -name '*.go' -o -name '*.mod' \\)",
			wantMaxDepth1: true,
		},
		{
			pattern:       "**/*.{js,ts,jsx,tsx}",
			wantArgs:      "\\( -name '*.js' -o -name '*.ts' -o -name '*.jsx' -o -name '*.tsx' \\)",
			wantMaxDepth1: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			gotArgs, gotMaxDepth1 := GlobToFindArgs(tt.pattern)
			if gotArgs != tt.wantArgs {
				t.Errorf("GlobToFindArgs(%q) args = %q, want %q", tt.pattern, gotArgs, tt.wantArgs)
			}
			if gotMaxDepth1 != tt.wantMaxDepth1 {
				t.Errorf("GlobToFindArgs(%q) maxDepth1 = %v, want %v", tt.pattern, gotMaxDepth1, tt.wantMaxDepth1)
			}
		})
	}
}
