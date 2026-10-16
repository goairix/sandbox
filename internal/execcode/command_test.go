package execcode

import "testing"

// A changed interpreter or shell-argument escape would execute different code.
func TestBuildCommand(t *testing.T) {
	for _, tc := range []struct {
		language string
		code     string
		want     string
	}{
		{"python", "print('quoted')", `python3 -c 'print('\''quoted'\'')'`},
		{"nodejs", "console.log('quoted')", `node -e 'console.log('\''quoted'\'')'`},
		{"bash", "printf '%s\\n' '$HOME'; exit 7", "printf '%s\\n' '$HOME'; exit 7"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			got, err := BuildCommand(tc.language, tc.code)
			if err != nil || got != tc.want {
				t.Fatalf("command = %q, error = %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := BuildCommand("ruby", "puts 1"); err == nil {
		t.Fatal("unsupported language accepted")
	}
}
