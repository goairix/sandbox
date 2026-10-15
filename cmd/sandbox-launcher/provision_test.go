package main

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
)

func TestEnrolledProvisionConsumesOnlyCanonicalPublicEnvironment(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	c := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"}, Roots: []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("a", 64), ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"}}
	wire, err := json.Marshal(c)
	require.NoError(t, err)
	t.Setenv(launcherconfig.Environment, string(wire))
	got, err := consumeProvisionedConfig()
	require.NoError(t, err)
	require.Equal(t, wire, got)
	_, present := os.LookupEnv(launcherconfig.Environment)
	require.False(t, present)
	for _, invalid := range []string{"", string(wire) + "\n", strings.Repeat("x", 16385), strings.Replace(string(wire), `"version":2`, `"version":2,"private_key":"secret"`, 1)} {
		t.Setenv(launcherconfig.Environment, invalid)
		_, err := consumeProvisionedConfig()
		require.Error(t, err)
	}
}

func TestEnrolledProvisionFixedModeRequiresActualPID1(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("host boundary test requires non-PID1")
	}
	require.Error(t, run([]string{"serve-provisioned"}))
	require.Error(t, run([]string{"serve-provisioned", "/tmp/config"}))
}
