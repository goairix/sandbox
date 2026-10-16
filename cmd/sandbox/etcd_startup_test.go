package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEtcdStartupModeRejectsIncompatibleFlagsBeforeCheck(t *testing.T) {
	for _, name := range []string{"config", "audit-workspace-owners", "recover-workspace-owner", "drain-release", "drain-kubernetes-deployment", "drain-kubernetes-namespace", "drain-kubernetes-hpa", "resume-kubernetes-deployment", "resume-kubernetes-namespace", "resume-kubernetes-replicas", "kubernetes-backend-fingerprint-namespace", "kubernetes-backend-fingerprint-deployment", "kubernetes-backend-fingerprint", "kubernetes-cleanup-protocol", "required-kubernetes-drain-protocol", "verify-kubernetes-backend-fingerprint", "drain-timeout"} {
		t.Run(name, func(t *testing.T) {
			called := false
			handled, err := runEtcdStartupCheck(context.Background(), map[string]bool{"check-etcd-startup": true, name: true}, nil, "manifest", func(context.Context, string) error { called = true; return nil })
			if !handled || err == nil || called {
				t.Fatalf("handled=%t err=%v called=%t", handled, err, called)
			}
		})
	}
}

func TestEtcdStartupMainEarly(t *testing.T) {
	if os.Getenv("SANDBOX_ETCD_CHECK_MAIN_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = append([]string{os.Args[0]}, os.Args[3:]...)
		main()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEtcdStartupMainEarly$", "--", "--check-etcd-startup", filepath.Join(dir, "missing-manifest"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "SANDBOX_ETCD_CHECK_MAIN_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "etcd startup check failed") || !strings.Contains(string(out), "manifest") || strings.Contains(string(out), "failed to load config") {
		t.Fatalf("main did not take early check branch: %v %s", err, out)
	}
}

func TestEtcdStartupModeCompletionAndDeadline(t *testing.T) {
	for _, tc := range []struct {
		path string
		args []string
	}{{"", nil}, {"manifest", []string{"extra"}}} {
		_, err := runEtcdStartupCheck(context.Background(), map[string]bool{"check-etcd-startup": true}, tc.args, tc.path, func(context.Context, string) error { t.Fatal("invalid mode invoked checker"); return nil })
		if err == nil {
			t.Fatal("accepted invalid check mode")
		}
	}
	want := errors.New("closed checker failure")
	handled, err := runEtcdStartupCheck(context.Background(), map[string]bool{"check-etcd-startup": true}, nil, "manifest", func(ctx context.Context, path string) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second || path != "manifest" {
			t.Fatal("missing bounded check context")
		}
		return want
	})
	if !handled || !errors.Is(err, want) {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	handled, err = runEtcdStartupCheck(context.Background(), nil, nil, "", func(context.Context, string) error { t.Fatal("ordinary startup invoked check"); return nil })
	if handled || err != nil {
		t.Fatal("ordinary startup changed")
	}
}
