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

func TestEtcdServeModeRejectsIncompatibleFlags(t *testing.T) {
	for _, name := range []string{"check-etcd-startup", "config", "audit-workspace-owners", "recover-workspace-owner", "drain-release", "drain-kubernetes-deployment", "drain-kubernetes-namespace", "drain-kubernetes-hpa", "resume-kubernetes-deployment", "resume-kubernetes-namespace", "resume-kubernetes-replicas", "kubernetes-backend-fingerprint-namespace", "kubernetes-backend-fingerprint-deployment", "kubernetes-backend-fingerprint", "kubernetes-cleanup-protocol", "required-kubernetes-drain-protocol", "verify-kubernetes-backend-fingerprint", "drain-timeout"} {
		t.Run(name, func(t *testing.T) {
			called := false
			handled, err := runEtcdServe(context.Background(), map[string]bool{"serve-etcd": true, name: true}, nil, "manifest", func(context.Context, string) error { called = true; return nil })
			if !handled || err == nil || called {
				t.Fatal("incompatible explicit startup flag reached HTTP startup")
			}
		})
	}
	for _, tc := range []struct {
		path string
		args []string
	}{{"", nil}, {"manifest", []string{"extra"}}} {
		handled, err := runEtcdServe(context.Background(), map[string]bool{"serve-etcd": true}, tc.args, tc.path, func(context.Context, string) error { t.Fatal("invalid mode invoked server"); return nil })
		if !handled || err == nil {
			t.Fatal("accepted serve mode without exactly one flag manifest")
		}
	}
}

func TestEtcdServeModeCompletionAndCancellation(t *testing.T) {
	want := errors.New("original serve failure")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled, err := runEtcdServe(ctx, map[string]bool{"serve-etcd": true}, nil, "manifest", func(got context.Context, path string) error {
		if got != ctx || path != "manifest" {
			t.Fatal("server lost the original lifecycle context or manifest")
		}
		return want
	})
	if !handled || !errors.Is(err, want) {
		t.Fatal("original server result was hidden")
	}
	cancel()
	handled, err = runEtcdServe(ctx, map[string]bool{"serve-etcd": true}, nil, "manifest", func(context.Context, string) error { t.Fatal("canceled startup invoked server"); return nil })
	if !handled || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled startup was not rejected before initialization")
	}
	handled, err = runEtcdServe(context.Background(), nil, nil, "", func(context.Context, string) error { t.Fatal("default startup invoked etcd server"); return nil })
	if handled || err != nil {
		t.Fatal("ordinary startup changed")
	}
}

func TestEtcdServeMainEarly(t *testing.T) {
	if os.Getenv("SANDBOX_ETCD_SERVE_MAIN_CHILD") == "1" {
		flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
		os.Args = append([]string{os.Args[0]}, os.Args[3:]...)
		main()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEtcdServeMainEarly$", "--", "--serve-etcd", filepath.Join(dir, "missing-manifest"))
	command.Dir = dir
	command.Env = append(os.Environ(), "SANDBOX_ETCD_SERVE_MAIN_CHILD=1")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "etcd HTTP serve failed") || !strings.Contains(string(output), "manifest") || strings.Contains(string(output), "failed to load config") || strings.Contains(string(output), "Redis initialization") {
		t.Fatalf("main did not take the standalone HTTP branch before legacy configuration: %v %s", err, output)
	}
}
