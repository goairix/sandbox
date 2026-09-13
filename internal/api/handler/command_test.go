package handler

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestBuildCommandPreservesInterpreterStdinAndLiteralCode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lang        sandbox.Language
		interpreter string
		code        string
	}{
		{name: "python", lang: sandbox.LangPython, interpreter: "python3", code: "import sys\n# SANDBOX_EOF\nprint(\"引号 ' \\\" $HOME $(echo unsafe) `echo unsafe`\")\nprint(sys.stdin.read(), end='')"},
		{name: "node", lang: sandbox.LangNodeJS, interpreter: "node", code: "// SANDBOX_EOF\nconsole.log(\"引号 ' \\\" $HOME $(echo unsafe) `echo unsafe`\");\nprocess.stdout.write(require('fs').readFileSync(0, 'utf8'));"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := exec.LookPath(tc.interpreter); err != nil {
				t.Skipf("%s unavailable: %v", tc.interpreter, err)
			}
			command, err := buildCommand(tc.lang, tc.code)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", command)
			cmd.Stdin = strings.NewReader("stdin 数据\n")
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
			require.Equal(t, "引号 ' \" $HOME $(echo unsafe) `echo unsafe`\nstdin 数据\n", string(output))
		})
	}
}

func TestBuildCommandDoesNotInterpretHeredocMarkerInCode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lang        sandbox.Language
		prefix      string
		interpreter string
		code        string
	}{
		{name: "python", lang: sandbox.LangPython, prefix: "python3 -c ", interpreter: "python3", code: "print('''SANDBOX_EOF\nprintf shell-injected\n''', end='')"},
		{name: "node", lang: sandbox.LangNodeJS, prefix: "node -e ", interpreter: "node", code: "process.stdout.write(`SANDBOX_EOF\nprintf shell-injected\n`);"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command, err := buildCommand(tc.lang, tc.code)
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(command, tc.prefix), command)
			if _, err := exec.LookPath(tc.interpreter); err != nil {
				t.Skipf("%s unavailable: %v", tc.interpreter, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
			require.NoError(t, err, string(output))
			require.Equal(t, "SANDBOX_EOF\nprintf shell-injected\n", string(output))
		})
	}
}
