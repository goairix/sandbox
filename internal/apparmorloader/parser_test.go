package apparmorloader

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func init() {
	mode := os.Getenv("SANDBOX_PARSER_HELPER")
	if mode == "" {
		return
	}
	if mode == "timeout" {
		time.Sleep(time.Second)
		os.Exit(0)
	}
	if mode == "fail" {
		_, _ = os.Stderr.WriteString("sensitive policy body")
		os.Exit(2)
	}
	expected := []string{"--add", "--skip-cache"}
	if mode == "syntax" {
		expected = append(expected, "--skip-kernel-load")
	}
	if strings.Join(os.Args[1:], " ") != strings.Join(expected, " ") {
		os.Exit(3)
	}
	policy, _ := io.ReadAll(os.Stdin)
	if string(policy) != testTemplate {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestParserFixedInvocation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"load", "syntax", "fail", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SANDBOX_PARSER_HELPER", mode)
			t.Setenv("GORACE", "atexit_sleep_ms=0")
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*100)
			defer cancel()
			err := runParser(ctx, binary, []byte(testTemplate), mode == "load")
			if mode == "load" || mode == "syntax" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected parser error")
			}
			if mode == "fail" && strings.Contains(err.Error(), "sensitive") {
				t.Fatal("parser output leaked")
			}
			if mode == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout err=%v", err)
			}
		})
	}
}

func TestExecParserMissingTrustedBinary(t *testing.T) {
	if _, err := os.Stat("/sbin/apparmor_parser"); err == nil {
		t.Skip("real parser is installed; no kernel mutation in unit tests")
	}
	if err := ExecParser(context.Background(), []byte(testTemplate), false); err == nil {
		t.Fatal("missing trusted parser accepted")
	}
}
