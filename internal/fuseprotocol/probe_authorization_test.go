package fuseprotocol

import (
	"bytes"
	"crypto/ed25519"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestProbeControlAuthorizationBindsCommandUIDAndGeneration(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x6a}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	request, err := SignProbeControl(key, "cleanup", "uid-a", 7)
	require.NoError(t, err)
	require.True(t, VerifyProbeControl(public, request, "cleanup", "uid-a", 7))
	require.False(t, VerifyProbeControl(public, request, "register", "uid-a", 7))
	require.False(t, VerifyProbeControl(public, request, "cleanup", "uid-b", 7))
	require.False(t, VerifyProbeControl(public, request, "cleanup", "uid-a", 999))
	for _, mutate := range []func(*ProbeControlRequest){
		func(r *ProbeControlRequest) { r.Version++ },
		func(r *ProbeControlRequest) { r.Authorization = "" },
		func(r *ProbeControlRequest) { r.Generation = 999 },
		func(r *ProbeControlRequest) { r.RuntimeUID = "uid-b" },
		func(r *ProbeControlRequest) { r.Command = "register" },
	} {
		forged := request
		mutate(&forged)
		require.False(t, VerifyProbeControl(public, forged, "cleanup", forged.RuntimeUID, forged.Generation))
	}
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7b}, ed25519.SeedSize))
	require.False(t, VerifyProbeControl(other.Public().(ed25519.PublicKey), request, "cleanup", "uid-a", 7))
	_, err = SignProbeControl(nil, "cleanup", "uid-a", 7)
	require.Error(t, err)
	_, err = SignProbeControl(key, "resume", "uid-a", 7)
	require.Error(t, err)
}
