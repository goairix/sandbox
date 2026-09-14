package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/redisbootstrap"
)

func memberCommandFixture(t *testing.T, command string) []string {
	t.Helper()
	secret, err := redisbootstrap.GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := redisbootstrap.ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	c := redisbootstrap.ClusterState{ClusterID: "isolated", Phase: redisbootstrap.Pending, Members: [3]string{"redis-0.local", "redis-1.local", "redis-2.local"}}
	digest, err := redisbootstrap.PublicKeySetDigest(keys)
	if err != nil {
		t.Fatal(err)
	}
	r := redisbootstrap.BootstrapRegistration{Cluster: c, KeyDigest: digest, MarkerIDs: [3]string{strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)}}
	dir := t.TempDir()
	control := t.TempDir()
	identity := redisbootstrap.VolumeIdentity{ClusterID: c.ClusterID, MarkerID: r.MarkerIDs[1], Member: redisbootstrap.Member{DNS: c.Members[1], Ordinal: 1}, InitialConfig: redisbootstrap.Reserved}
	if err := redisbootstrap.WriteVolumeIdentity(context.Background(), filepath.Join(dir, "identity.json"), identity, c); err != nil {
		t.Fatal(err)
	}
	dataPassword := strings.Repeat("d", 40)
	sentinelPassword := strings.Repeat("s", 40)
	o := redisbootstrap.InitialConfigOptions{MasterName: "sandbox", DataPassword: dataPassword, SentinelPassword: sentinelPassword, DownAfterMilliseconds: 10000, FailoverTimeoutMilliseconds: 60000, ParallelSyncs: 1}
	if _, err := redisbootstrap.ConfigureInitialLocalVolume(context.Background(), dir, r, keys, identity.Member, o); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"cluster.json": c, "registration.json": r} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(control, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(control, "keys.json"), secret.Data["public-keys.json"], 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REDIS_PASSWORD", dataPassword)
	t.Setenv("REDIS_SENTINEL_PASSWORD", sentinelPassword)
	return []string{command, "-data-dir", dir, "-cluster-file", filepath.Join(control, "cluster.json"), "-registration-file", filepath.Join(control, "registration.json"), "-public-keys-file", filepath.Join(control, "keys.json"), "-ordinal", "1"}
}

func TestMemberCommandExecFixedArgv(t *testing.T) {
	for _, command := range []string{"redis", "sentinel"} {
		t.Run(command, func(t *testing.T) {
			args := memberCommandFixture(t, command)
			var output bytes.Buffer
			called := false
			oldMask := syscall.Umask(0077)
			defer syscall.Umask(oldMask)
			err := runMemberWithExec(context.Background(), args, &output, func(path string, argv, env []string) error {
				called = true
				want := []string{"redis-server", filepath.Join(args[2], "redis.conf")}
				if command == "sentinel" {
					want[1] = filepath.Join(args[2], "sentinel.conf")
					want = append(want, "--sentinel")
				}
				if path != "/usr/local/bin/redis-server" || !reflect.DeepEqual(argv, want) || !reflect.DeepEqual(env, os.Environ()) {
					t.Fatal("exec was not fixed direct Redis argv")
				}
				for _, arg := range argv {
					if strings.Contains(arg, os.Getenv("REDIS_PASSWORD")) || strings.Contains(arg, "sh") || strings.Contains(arg, "sleep") {
						t.Fatal("credential or shell in argv")
					}
				}
				mask := syscall.Umask(0077)
				if mask != 0077 {
					t.Fatal("exec missing private umask")
				}
				return nil
			})
			if err != nil || !called || output.Len() != 0 {
				t.Fatal("member command did not exec", err)
			}
		})
	}
}

func TestMemberCommandHelpAndOrdinalEnvironment(t *testing.T) {
	for _, command := range []string{"redis", "sentinel"} {
		var output bytes.Buffer
		if err := runMemberWithExec(context.Background(), []string{command, "--help"}, &output, nil); err != nil || !strings.Contains(output.String(), "environment only") {
			t.Fatal("missing static help", err)
		}
		args := memberCommandFixture(t, command)
		args = args[:len(args)-2]
		t.Setenv("POD_ORDINAL", "1")
		oldMask := syscall.Umask(0077)
		err := runMemberWithExec(context.Background(), args, &bytes.Buffer{}, func(string, []string, []string) error { return nil })
		syscall.Umask(oldMask)
		if err != nil {
			t.Fatal("stable ordinal env rejected", err)
		}
	}
}

func TestMemberCommandRejectsUnsafeArgsAndCancelsBeforeExec(t *testing.T) {
	for _, extra := range [][]string{{"-password", "do_not_echo_secret"}, {"-executable", "/bin/sh"}, {"do_not_echo_secret"}, {"-ordinal", "3"}, {"-ordinal", "-1"}} {
		args := memberCommandFixture(t, "redis")
		args = append(args, extra...)
		called := false
		var output bytes.Buffer
		if err := runMemberWithExec(context.Background(), args, &output, func(string, []string, []string) error { called = true; return nil }); err == nil || called || strings.Contains(output.String(), "do_not_echo_secret") {
			t.Fatal("unsafe args exposed or executed")
		}
	}
	args := memberCommandFixture(t, "redis")
	args = replaceAttestorArg(args, "-data-dir", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	called := false
	if err := runMemberWithExec(ctx, args, &bytes.Buffer{}, func(string, []string, []string) error { called = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("waiting command ignored cancellation", err)
	}
}
