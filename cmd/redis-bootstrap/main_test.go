package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/redisbootstrap"
)

func TestAttestorHelp(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"attestor", "--help"}, &output); err != nil || output.Len() == 0 {
		t.Fatalf("missing safe help: %v", err)
	}
}

func TestBuiltinSentinelRollbackGuardIsStaticAndAlwaysDenied(t *testing.T) {
	t.Setenv("REDIS_PASSWORD", "secret_environment_do_not_echo")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"deny-rollback"}, &output); err == nil || !strings.Contains(output.String(), "use helm upgrade") || strings.Contains(output.String(), "secret_environment") {
		t.Fatal("rollback must be denied with static forward-upgrade guidance, without environment or state access")
	}
	output.Reset()
	if err := run(context.Background(), []string{"deny-rollback", "secret_argument_do_not_echo"}, &output); err == nil || output.Len() != 0 {
		t.Fatal("rollback guard must reject and not echo extra arguments")
	}
}

func replaceAttestorArg(args []string, name, value string) []string {
	result := append([]string(nil), args...)
	for i := 1; i+1 < len(result); i += 2 {
		if result[i] == name {
			result[i+1] = value
			return result
		}
	}
	return append(result, name, value)
}

func TestAttestorRejectsUnsafeInput(t *testing.T) {
	cases := map[string]func(*testing.T, []string) []string{
		"missing ordinal": func(_ *testing.T, args []string) []string { return replaceAttestorArg(args, "-ordinal", "-1") },
		"outside ordinal": func(_ *testing.T, args []string) []string { return replaceAttestorArg(args, "-ordinal", "3") },
		"relative path": func(_ *testing.T, args []string) []string {
			return replaceAttestorArg(args, "-cluster-file", "relative")
		},
		"root PVC": func(_ *testing.T, args []string) []string { return replaceAttestorArg(args, "-data-dir", "/") },
		"master injection": func(_ *testing.T, args []string) []string {
			return replaceAttestorArg(args, "-master-name", "sandbox\ninclude evil")
		},
		"missing password": func(t *testing.T, args []string) []string { t.Setenv("REDIS_PASSWORD", ""); return args },
		"unsafe password": func(t *testing.T, args []string) []string {
			t.Setenv("REDIS_PASSWORD", strings.Repeat("x", 32)+" quote\"")
			return args
		},
		"password argv": func(_ *testing.T, args []string) []string {
			return append(args, "-password", "secret_argument_do_not_echo")
		},
		"unknown endpoint": func(_ *testing.T, args []string) []string { return append(args, "-endpoint", "http://evil") },
		"extra positional": func(_ *testing.T, args []string) []string { return append(args, "secret_argument_do_not_echo") },
		"wrong seed member": func(_ *testing.T, args []string) []string {
			return replaceAttestorArg(args, "-ordinal", "2")
		},
		"corrupt public keys": func(t *testing.T, args []string) []string {
			for i := range args {
				if args[i] == "-public-keys-file" {
					if err := os.WriteFile(args[i+1], []byte("null"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return args
		},
		"long seed": func(t *testing.T, args []string) []string {
			for i := range args {
				if args[i] == "-private-seed-file" {
					if err := os.WriteFile(args[i+1], []byte(strings.Repeat("z", 64)), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return args
		},
		"public seed mode": func(t *testing.T, args []string) []string {
			for i := range args {
				if args[i] == "-private-seed-file" {
					if err := os.Chmod(args[i+1], 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			return args
		},
		"group writable seed": func(t *testing.T, args []string) []string {
			for i := range args {
				if args[i] == "-private-seed-file" {
					if err := os.Chmod(args[i+1], 0660); err != nil {
						t.Fatal(err)
					}
				}
			}
			return args
		},
		"nonregular file": func(t *testing.T, args []string) []string {
			return replaceAttestorArg(args, "-private-seed-file", t.TempDir())
		},
		"invalid registration": func(t *testing.T, args []string) []string {
			for i := range args {
				if args[i] == "-registration-file" {
					if err := os.WriteFile(args[i+1], []byte("{}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			return args
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			args, _, _ := attestorFiles(t)
			args = mutate(t, args)
			called := false
			var output bytes.Buffer
			err := runWithServer(context.Background(), args, &output, func(context.Context, redisbootstrap.IdentityServerOptions) error { called = true; return nil })
			if err == nil || called {
				t.Fatal("unsafe identity runtime accepted")
			}
			if output.Len() != 0 || strings.Contains(err.Error(), "secret_argument_do_not_echo") || strings.Contains(err.Error(), os.Getenv("REDIS_PASSWORD")) && os.Getenv("REDIS_PASSWORD") != "" {
				t.Fatal("raw arguments or credentials exposed")
			}
		})
	}
}

func TestAttestorInitializedRegistrationAndProjection(t *testing.T) {
	args, cluster, _ := attestorFiles(t)
	cluster.Phase = redisbootstrap.Initialized
	var clusterPath, publicPath, registrationPath, seedPath string
	for i := range args {
		switch args[i] {
		case "-cluster-file":
			clusterPath = args[i+1]
		case "-public-keys-file":
			publicPath = args[i+1]
		case "-registration-file":
			registrationPath = args[i+1]
		case "-private-seed-file":
			seedPath = args[i+1]
		}
	}
	clusterJSON, err := json.Marshal(cluster)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clusterPath, clusterJSON, 0600); err != nil {
		t.Fatal(err)
	}
	called := 0
	serve := func(context.Context, redisbootstrap.IdentityServerOptions) error { called++; return nil }
	if err := runWithServer(context.Background(), args, &bytes.Buffer{}, serve); err == nil || called != 0 {
		t.Fatal("Initialized accepted missing registration")
	}
	publicJSON, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := redisbootstrap.ParseMemberPublicKeys(publicJSON)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := redisbootstrap.PublicKeySetDigest(keys)
	if err != nil {
		t.Fatal(err)
	}
	registration := redisbootstrap.BootstrapRegistration{Cluster: cluster, KeyDigest: digest, MarkerIDs: [3]string{strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)}}
	writeRegistration := func() {
		data, err := json.Marshal(registration)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(registrationPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeRegistration()
	if err := os.Chmod(seedPath, 0440); err != nil {
		t.Fatal(err)
	}
	// Trust Kubernetes control-plane projections: their final file is a symlink
	// into ..data. This is intentionally different from the untrusted PVC reader.
	projected := filepath.Join(t.TempDir(), "cluster.json")
	if err := os.Symlink(clusterPath, projected); err != nil {
		t.Fatal(err)
	}
	args = replaceAttestorArg(args, "-cluster-file", projected)
	if err := runWithServer(context.Background(), args, &bytes.Buffer{}, serve); err != nil || called != 1 {
		t.Fatalf("valid Initialized projection rejected: %v", err)
	}
	registration.KeyDigest = strings.Repeat("0", 64)
	writeRegistration()
	if err := runWithServer(context.Background(), args, &bytes.Buffer{}, serve); err == nil || called != 1 {
		t.Fatal("changed registered keys accepted")
	}
	registration.KeyDigest = digest
	registration.Cluster.ClusterID = "different"
	writeRegistration()
	if err := runWithServer(context.Background(), args, &bytes.Buffer{}, serve); err == nil || called != 1 {
		t.Fatal("changed registered cluster accepted")
	}
}

func TestAttestorCanceled(t *testing.T) {
	args, _, _ := attestorFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := runWithServer(ctx, args, &bytes.Buffer{}, func(context.Context, redisbootstrap.IdentityServerOptions) error { called = true; return nil }); err != context.Canceled || called {
		t.Fatal("canceled startup executed")
	}
}

func TestAttestorPreservesCancellationDuringServerFailure(t *testing.T) {
	args, _, _ := attestorFiles(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := runWithServer(ctx, args, &bytes.Buffer{}, func(context.Context, redisbootstrap.IdentityServerOptions) error {
		cancel()
		return errors.New("internal shutdown failure")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
}

func TestAttestorControlFileBoundaries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "private")
	for _, mode := range []os.FileMode{0600, 0440, 0400} {
		if err := os.WriteFile(path, []byte(strings.Repeat("a", 32)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if data, err := readControlFile(ctx, path, 32, true); err != nil || len(data) != 32 {
			t.Fatalf("valid seed projection mode rejected: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 33)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readControlFile(ctx, path, 32, true); err == nil {
		t.Fatal("oversize seed accepted")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := readControlFile(ctx, fifo, 64<<10, false); err == nil {
		t.Fatal("FIFO accepted as projected state")
	}
	if time.Since(start) > time.Second {
		t.Fatal("FIFO open blocked")
	}
	if _, err := readControlFile(ctx, filepath.Join(dir, "missing"), 32, true); !errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), dir) {
		t.Fatal("missing-file error lost type or exposed path")
	}
	//nolint:staticcheck // Deliberately invalid context tests fail-closed rejection.
	if err := runWithServer(nil, []string{"attestor"}, &bytes.Buffer{}, func(context.Context, redisbootstrap.IdentityServerOptions) error { return nil }); err == nil {
		t.Fatal("nil context accepted")
	}
}

func attestorFiles(t *testing.T) ([]string, redisbootstrap.ClusterState, ed25519.PublicKey) {
	t.Helper()
	dir := t.TempDir()
	secret, err := redisbootstrap.GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := redisbootstrap.ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	c := redisbootstrap.ClusterState{ClusterID: "isolated", Phase: redisbootstrap.Pending, Members: [3]string{"redis-0.local", "redis-1.local", "redis-2.local"}}
	cluster, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"cluster.json", cluster}, {"public-keys.json", secret.Data["public-keys.json"]}, {"seed", secret.Data["redis-sentinel-1"]}} {
		if err := os.WriteFile(filepath.Join(dir, file.name), file.data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"attestor", "-data-dir", dir, "-cluster-file", filepath.Join(dir, "cluster.json"), "-registration-file", filepath.Join(dir, "registration.json"), "-public-keys-file", filepath.Join(dir, "public-keys.json"), "-private-seed-file", filepath.Join(dir, "seed"), "-ordinal", "1", "-master-name", "sandbox"}
	t.Setenv("REDIS_PASSWORD", "dummy_test_password_0123456789_abcdef")
	return args, c, keys[1]
}

func TestAttestorReadsRealMemberFiles(t *testing.T) {
	args, cluster, public := attestorFiles(t)
	called := false
	var output bytes.Buffer
	err := runWithServer(context.Background(), args, &output, func(ctx context.Context, o redisbootstrap.IdentityServerOptions) error {
		called = true
		if ctx == nil || o.Observer.Cluster != cluster || o.Observer.Member != (redisbootstrap.Member{DNS: cluster.Members[1], Ordinal: 1}) || o.Observer.Password != os.Getenv("REDIS_PASSWORD") || o.Observer.MasterName != "sandbox" || !bytes.Equal(o.PrivateKey.Public().(ed25519.PublicKey), public) {
			t.Fatal("wrong local observer identity")
		}
		return nil
	})
	if err != nil || !called || output.Len() != 0 {
		t.Fatalf("fixed member runtime not started: %v", err)
	}
}

func TestAttestorOrdinalEnvironmentFallbackAndExplicitOverride(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(strconv.FormatBool(explicit), func(t *testing.T) {
			args, c, _ := attestorFiles(t)
			if explicit {
				t.Setenv("POD_ORDINAL", "2")
			} else {
				t.Setenv("POD_ORDINAL", "1")
				filtered := []string{args[0]}
				for i := 1; i < len(args); i += 2 {
					if args[i] != "-ordinal" {
						filtered = append(filtered, args[i], args[i+1])
					}
				}
				args = filtered
			}
			called := false
			err := runWithServer(context.Background(), args, &bytes.Buffer{}, func(ctx context.Context, o redisbootstrap.IdentityServerOptions) error {
				called = true
				if o.Observer.Member.Ordinal != 1 || o.Observer.Member.DNS != c.Members[1] {
					t.Fatal("ordinal fallback/override selected wrong fixed member")
				}
				return nil
			})
			if err != nil || !called {
				t.Fatal("backward-compatible attestor ordinal failed", err)
			}
		})
	}
}

func TestAttestorRejectsMalformedOrdinalEnvironment(t *testing.T) {
	for _, value := range []string{"", "01", "+1", "3", "-1", "1\n"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			t.Setenv("POD_ORDINAL", value)
			called := false
			var output bytes.Buffer
			if err := runWithServer(context.Background(), []string{"attestor", "-data-dir", "/missing-test-PVC", "-private-seed-file", "/missing-test-seed"}, &output, func(context.Context, redisbootstrap.IdentityServerOptions) error { called = true; return nil }); !errors.Is(err, errArguments) || called || output.Len() != 0 {
				t.Fatal("malformed ordinal env reached file loading/server", err)
			}
		})
	}
}
