package apparmorloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	rendered, name, digest := testPolicy()
	cfg := Config{ProfilePath: filepath.Join(root, "policy"), ProfileName: name, ProfileDigest: digest, ModuleEnabledPath: filepath.Join(root, "enabled"), ProfilesPath: filepath.Join(root, "profiles"), ReadinessFile: filepath.Join(root, "ready"), ProcRoot: filepath.Join(root, "proc"), CheckInterval: time.Millisecond * 10, ParserTimeout: time.Second}
	write(t, cfg.ProfilePath, rendered)
	write(t, cfg.ModuleEnabledPath, "Y\n")
	write(t, cfg.ProfilesPath, name+" (enforce)\n")
	proc := filepath.Join(cfg.ProcRoot, fmt.Sprint(os.Getpid()))
	if err := os.MkdirAll(proc, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(proc, "stat"), fmt.Sprintf("%d (loader) S %s 42\n", os.Getpid(), strings.Repeat("0 ", 18)))
	return cfg
}
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	for _, tt := range []struct {
		name, module, profiles string
		parseErr               bool
		wantErr                bool
		loads                  int
	}{
		{"existing enforce", "Y", " (enforce)", false, false, 0},
		{"disabled", "N", " (enforce)", false, true, 0},
		{"complain", "Y", " (complain)", false, true, 0},
		{"parse failure", "Y", " (enforce)", true, true, 0},
		{"missing reload", "Y", "", false, false, 1},
		{"duplicate exact", "Y", " (enforce)\n%s (enforce)", false, true, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fixture(t)
			write(t, cfg.ModuleEnabledPath, tt.module)
			profiles := ""
			if tt.profiles != "" {
				profiles = cfg.ProfileName + strings.ReplaceAll(tt.profiles, "%s", cfg.ProfileName) + "\n"
			}
			write(t, cfg.ProfilesPath, profiles)
			loads := 0
			validates := 0
			parser := func(ctx context.Context, p []byte, load bool) error {
				if tt.parseErr {
					return errors.New("parser failed")
				}
				if load {
					loads++
					write(t, cfg.ProfilesPath, cfg.ProfileName+" (enforce)\n")
				} else {
					validates++
				}
				return nil
			}
			loader, err := New(cfg, parser)
			if err != nil {
				t.Fatal(err)
			}
			err = loader.Check(context.Background())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v", err)
			}
			if loads != tt.loads {
				t.Fatalf("loads=%d", loads)
			}
			probeErr := Ready(cfg.ReadinessFile, cfg.ProcRoot, time.Second)
			if (probeErr != nil) != tt.wantErr {
				t.Fatalf("readiness error=%v", probeErr)
			}
			if !tt.wantErr {
				if err = loader.Check(context.Background()); err != nil {
					t.Fatal(err)
				}
				if loads != tt.loads || validates != 1 {
					t.Fatalf("steady state parser calls load=%d syntax=%d", loads, validates)
				}
			}
		})
	}
}

func TestCheckHealthTransitionsAndImmutablePolicy(t *testing.T) {
	cfg := fixture(t)
	l, err := New(cfg, func(context.Context, []byte, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	write(t, cfg.ModuleEnabledPath, "N")
	if err = l.Check(context.Background()); err == nil {
		t.Fatal("disabled module accepted")
	}
	if err = Ready(cfg.ReadinessFile, cfg.ProcRoot, time.Second); err == nil {
		t.Fatal("stale ready")
	}
	write(t, cfg.ModuleEnabledPath, "Y")
	if err = l.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	rendered, _, _ := testPolicy()
	write(t, cfg.ProfilePath, rendered+"# changed\n")
	if err = l.Check(context.Background()); err == nil {
		t.Fatal("mutated profile accepted")
	}
	if _, err = os.Stat(cfg.ReadinessFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readiness not removed: %v", err)
	}
}

func TestCheckMissingRetryAndTimeout(t *testing.T) {
	cfg := fixture(t)
	write(t, cfg.ProfilesPath, "")
	calls := 0
	l, err := New(cfg, func(ctx context.Context, p []byte, load bool) error {
		if !load {
			return nil
		}
		calls++
		if calls == 1 {
			return errors.New("failed")
		}
		write(t, cfg.ProfilesPath, cfg.ProfileName+" (enforce)\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Check(context.Background()); err == nil {
		t.Fatal("parser failure accepted")
	}
	if err = l.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	cfg.ParserTimeout = time.Millisecond * 5
	l, err = New(cfg, func(ctx context.Context, _ []byte, _ bool) error { <-ctx.Done(); return ctx.Err() })
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Check(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err=%v", err)
	}
}

func TestRunCancellation(t *testing.T) {
	cfg := fixture(t)
	loads := 0
	l, err := New(cfg, func(context.Context, []byte, bool) error { loads++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for Ready(cfg.ReadinessFile, cfg.ProcRoot, time.Second) != nil {
		if time.Now().After(deadline) {
			t.Fatal("not ready")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = os.Stat(cfg.ReadinessFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ready remains")
	}
	if loads != 1 {
		t.Fatalf("unexpected parser call: %d", loads)
	}
}

func TestReadinessFreshnessAndIdentity(t *testing.T) {
	cfg := fixture(t)
	l, err := New(cfg, func(context.Context, []byte, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = Ready(cfg.ReadinessFile, cfg.ProcRoot, time.Nanosecond); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	write(t, filepath.Join(cfg.ProcRoot, fmt.Sprint(os.Getpid()), "stat"), fmt.Sprintf("%d (loader) S %s 43\n", os.Getpid(), strings.Repeat("0 ", 18)))
	if err = Ready(cfg.ReadinessFile, cfg.ProcRoot, time.Second); err == nil {
		t.Fatal("reused process accepted")
	}
}

func TestCheckKernelReadFailuresAndOnlyOwnProfile(t *testing.T) {
	for _, kind := range []string{"module unreadable", "profiles unreadable", "other profile", "missing after load", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			cfg := fixture(t)
			loads := 0
			l, err := New(cfg, func(_ context.Context, p []byte, load bool) error {
				if string(p) != string(CanonicalPolicy([]byte(strings.ReplaceAll(testTemplate, ProfilePlaceholder, cfg.ProfileName)))) {
					t.Fatal("unexpected policy")
				}
				if load {
					loads++
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = l.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			switch kind {
			case "module unreadable":
				if err = os.Remove(cfg.ModuleEnabledPath); err != nil {
					t.Fatal(err)
				}
			case "profiles unreadable":
				if err = os.Remove(cfg.ProfilesPath); err != nil {
					t.Fatal(err)
				}
			case "other profile":
				write(t, cfg.ProfilesPath, cfg.ProfileName+"-other (enforce)\n")
			case "missing after load":
				write(t, cfg.ProfilesPath, "")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err = l.Check(ctx); err == nil {
				t.Fatal("failure accepted")
			}
			if Ready(cfg.ReadinessFile, cfg.ProcRoot, time.Second) == nil {
				t.Fatal("failed check retained Ready")
			}
			wantLoads := 0
			if kind == "other profile" || kind == "missing after load" {
				wantLoads = 1
			}
			if loads != wantLoads {
				t.Fatalf("loaded own profile %d times, want %d", loads, wantLoads)
			}
		})
	}
}

func TestReadinessStoppedProcess(t *testing.T) {
	cfg := fixture(t)
	l, err := New(cfg, func(context.Context, []byte, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.ProcRoot, fmt.Sprint(os.Getpid()), "stat")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.Replace(string(contents), ") S ", ") T ", 1))
	if Ready(cfg.ReadinessFile, cfg.ProcRoot, time.Second) == nil {
		t.Fatal("stopped process accepted")
	}
}
