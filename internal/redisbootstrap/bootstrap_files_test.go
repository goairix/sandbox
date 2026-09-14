package redisbootstrap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func bootstrapFilesFixture(t *testing.T) (BootstrapFilePaths, BootstrapRegistration) {
	t.Helper()
	r, keys, _ := initialConfigFixture(t)
	dir := t.TempDir()
	paths := BootstrapFilePaths{filepath.Join(dir, "cluster.json"), filepath.Join(dir, "registration.json"), filepath.Join(dir, "public-keys.json")}
	cluster, err := json.Marshal(r.Cluster)
	if err != nil {
		t.Fatal(err)
	}
	var public [3]string
	for i, key := range keys {
		public[i] = hex.EncodeToString(key)
	}
	publicJSON, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Cluster, cluster, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.PublicKeys, publicJSON, 0600); err != nil {
		t.Fatal(err)
	}
	return paths, r
}

func TestBootstrapFilesPendingAndInitialized(t *testing.T) {
	paths, r := bootstrapFilesFixture(t)
	loaded, err := ReadBootstrapFiles(context.Background(), paths)
	if err != nil || loaded.Cluster != r.Cluster || loaded.Registration != nil {
		t.Fatalf("fresh Pending files not loaded: %v", err)
	}
	digest, err := PublicKeySetDigest(loaded.PublicKeys)
	if err != nil || digest != r.KeyDigest {
		t.Fatal("public set not preserved")
	}
	r.Cluster.Phase = Initialized
	cluster, _ := json.Marshal(r.Cluster)
	registration, _ := json.Marshal(r)
	if err := os.WriteFile(paths.Cluster, cluster, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Registration, registration, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err = ReadBootstrapFiles(context.Background(), paths)
	if err != nil || !reflect.DeepEqual(loaded.Registration, &r) {
		t.Fatalf("registered identity not loaded: %v", err)
	}
	if err := WaitBootstrapInitialized(context.Background(), paths); err != nil {
		t.Fatalf("Initialized gate stayed closed: %v", err)
	}
}

func TestBootstrapFilesPendingGateDoesNotOpen(t *testing.T) {
	paths, _ := bootstrapFilesFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := WaitBootstrapInitialized(ctx, paths); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pending gate returned before deadline: %v", err)
	}
}

func TestBootstrapFilesRejectMalformedAndMismatchedFiles(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, BootstrapFilePaths, BootstrapRegistration){
		"initialized no registration": func(t *testing.T, p BootstrapFilePaths, r BootstrapRegistration) {
			r.Cluster.Phase = Initialized
			b, _ := json.Marshal(r.Cluster)
			if err := os.WriteFile(p.Cluster, b, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"wrong phase": func(t *testing.T, p BootstrapFilePaths, r BootstrapRegistration) {
			r.Cluster.Phase = Initialized
			b, _ := json.Marshal(r)
			if err := os.WriteFile(p.Registration, b, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"wrong keyset": func(t *testing.T, p BootstrapFilePaths, r BootstrapRegistration) {
			r.KeyDigest = strings.Repeat("a", 64)
			b, _ := json.Marshal(r)
			if err := os.WriteFile(p.Registration, b, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"empty registration": func(t *testing.T, p BootstrapFilePaths, _ BootstrapRegistration) {
			if err := os.WriteFile(p.Registration, nil, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"oversized registration": func(t *testing.T, p BootstrapFilePaths, _ BootstrapRegistration) {
			if err := os.WriteFile(p.Registration, []byte(strings.Repeat("s", maximumStateBytes+1)), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"registration directory": func(t *testing.T, p BootstrapFilePaths, _ BootstrapRegistration) {
			if err := os.Mkdir(p.Registration, 0700); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, p BootstrapFilePaths, _ BootstrapRegistration) {
			if err := syscall.Mkfifo(p.Registration, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"public oversized": func(t *testing.T, p BootstrapFilePaths, _ BootstrapRegistration) {
			if err := os.WriteFile(p.PublicKeys, []byte(strings.Repeat("s", 1025)), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"malformed public": func(t *testing.T, p BootstrapFilePaths, _ BootstrapRegistration) {
			if err := os.WriteFile(p.PublicKeys, []byte(`["secret-not-a-key","secret-not-a-key","secret-not-a-key"]`), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"cluster oversized": func(t *testing.T, p BootstrapFilePaths, _ BootstrapRegistration) {
			if err := os.WriteFile(p.Cluster, []byte(strings.Repeat("s", maximumStateBytes+1)), 0600); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			paths, r := bootstrapFilesFixture(t)
			mutate(t, paths, r)
			loaded, err := ReadBootstrapFiles(context.Background(), paths)
			if err == nil || err.Error() != errBootstrapFiles.Error() || !reflect.DeepEqual(loaded, BootstrapFiles{}) {
				t.Fatalf("unconfirmed projection accepted/leaked: %v", err)
			}
		})
	}
}

func TestBootstrapFilesKubernetesProjectionSymlinks(t *testing.T) {
	paths, r := bootstrapFilesFixture(t)
	dir := filepath.Dir(paths.Cluster)
	generation := filepath.Join(dir, "..generation")
	if err := os.Mkdir(generation, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cluster.json", "public-keys.json"} {
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(generation, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("..generation", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadBootstrapFiles(context.Background(), paths)
	if err != nil || loaded.Cluster != r.Cluster {
		t.Fatalf("trusted ..data projection rejected: %v", err)
	}
}

func TestBootstrapFilesContextAndPathValidation(t *testing.T) {
	paths, _ := bootstrapFilesFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loaded, err := ReadBootstrapFiles(ctx, paths)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(loaded, BootstrapFiles{}) {
		t.Fatal("cancellation lost", err)
	}
	for _, p := range []BootstrapFilePaths{{Cluster: "/", Registration: paths.Registration, PublicKeys: paths.PublicKeys}, {Cluster: "relative", Registration: paths.Registration, PublicKeys: paths.PublicKeys}, {Cluster: paths.Cluster, Registration: paths.Cluster, PublicKeys: paths.PublicKeys}} {
		if _, err := ReadBootstrapFiles(context.Background(), p); err == nil {
			t.Fatal("invalid path accepted")
		}
	}
	var absentContext context.Context
	if _, err := ReadBootstrapFiles(absentContext, paths); err == nil {
		t.Fatal("nil context accepted")
	}
	if err := WaitBootstrapInitialized(absentContext, paths); err == nil {
		t.Fatal("nil waiting context accepted")
	}
}
