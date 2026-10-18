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
	for _, name := range []string{"initialize-etcd-inventory", "serve-etcd", "config", "audit-workspace-owners", "recover-workspace-owner", "drain-release", "drain-kubernetes-deployment", "drain-kubernetes-namespace", "drain-kubernetes-hpa", "resume-kubernetes-deployment", "resume-kubernetes-namespace", "resume-kubernetes-replicas", "kubernetes-backend-fingerprint-namespace", "kubernetes-backend-fingerprint-deployment", "kubernetes-backend-fingerprint", "kubernetes-cleanup-protocol", "required-kubernetes-drain-protocol", "verify-kubernetes-backend-fingerprint", "drain-timeout"} {
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

// Exercise the real CLI parser and dispatch before adding the operator entry.
// A missing command must fail on behavior, not an undeclared Go test symbol;
// these refusals do not stand in for initializing a real mounted inventory.
func TestEtcdInventoryMainExplicitOperatorMode(t *testing.T) {
	if os.Getenv("SANDBOX_ETCD_INVENTORY_MAIN_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = append([]string{os.Args[0]}, os.Args[3:]...)
		main()
		os.Exit(0)
	}

	dir := t.TempDir()
	manifest := filepath.Join(dir, "operator-private-path-sentinel.json")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing-mounted-manifest", []string{"--initialize-etcd-inventory=" + manifest}, "etcd inventory initialization failed"},
		{"empty-manifest", []string{"--initialize-etcd-inventory="}, "requires one manifest path"},
		{"positional", []string{"--initialize-etcd-inventory=" + manifest, "extra"}, "no positional arguments"},
		{"legacy-config", []string{"--initialize-etcd-inventory=" + manifest, "--config=private-config-sentinel"}, "cannot be combined"},
		{"read-only-check", []string{"--initialize-etcd-inventory=" + manifest, "--check-etcd-startup=other-manifest"}, "cannot be combined"},
		{"http-serve", []string{"--initialize-etcd-inventory=" + manifest, "--serve-etcd=other-manifest"}, "cannot be combined"},
		{"release-drain", []string{"--initialize-etcd-inventory=" + manifest, "--drain-release"}, "cannot be combined"},
		{"explicit-default-flag", []string{"--initialize-etcd-inventory=" + manifest, "--drain-timeout=10m"}, "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := append([]string{"-test.run=^TestEtcdInventoryMainExplicitOperatorMode$", "--"}, tc.args...)
			command := exec.CommandContext(ctx, os.Args[0], args...)
			command.Dir = dir
			command.Env = append(os.Environ(), "SANDBOX_ETCD_INVENTORY_MAIN_CHILD=1")
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("operator command did not exit within its refusal bound: %v", ctx.Err())
			}
			if err == nil || !strings.Contains(string(output), tc.want) {
				t.Fatalf("operator mode did not reject at the intended boundary: err=%v output=%s", err, output)
			}
			for _, forbidden := range []string{"failed to load config", "Redis initialization", "etcd HTTP serve failed", "operator-private-path-sentinel", "private-config-sentinel"} {
				if strings.Contains(string(output), forbidden) {
					t.Fatalf("operator refusal entered another startup mode or disclosed a mounted path: %s", output)
				}
			}
		})
	}
}

func TestEtcdInventoryModeHasNoImplicitStartupOrCancellationRetry(t *testing.T) {
	handled, err := runEtcdInventoryInitialization(context.Background(), nil, nil, "")
	if handled || err != nil {
		t.Fatalf("ordinary startup invoked operator mode: handled=%t err=%v", handled, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handled, err = runEtcdInventoryInitialization(ctx, map[string]bool{"initialize-etcd-inventory": true}, nil, filepath.Join(t.TempDir(), "missing-manifest"))
	if !handled || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled operator read configuration or lost cancellation: handled=%t err=%v", handled, err)
	}
	for _, name := range []string{"serve-etcd", "check-etcd-startup", "config", "audit-workspace-owners", "recover-workspace-owner", "drain-release", "drain-kubernetes-deployment", "drain-kubernetes-namespace", "drain-kubernetes-hpa", "resume-kubernetes-deployment", "resume-kubernetes-namespace", "resume-kubernetes-replicas", "kubernetes-backend-fingerprint-namespace", "kubernetes-backend-fingerprint-deployment", "kubernetes-backend-fingerprint", "kubernetes-cleanup-protocol", "required-kubernetes-drain-protocol", "verify-kubernetes-backend-fingerprint", "drain-timeout"} {
		t.Run(name, func(t *testing.T) {
			handled, err := runEtcdInventoryInitialization(ctx, map[string]bool{"initialize-etcd-inventory": true, name: true}, nil, "missing-manifest")
			if !handled || err == nil || !strings.Contains(err.Error(), "cannot be combined") || errors.Is(err, context.Canceled) {
				t.Fatalf("mode conflict was not rejected before loading: handled=%t err=%v", handled, err)
			}
		})
	}
}
