package sandboxsnapshot

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goairix/sandbox/pkg/types"
	"github.com/stretchr/testify/require"
)

func TestBusinessSnapshotStrictCodec(t *testing.T) {
	snapshot, err := Build(types.CreateSandboxRequest{Mode: "ephemeral"}, snapshotDefaults())
	require.NoError(t, err)
	wire, err := Encode(snapshot)
	require.NoError(t, err)
	for name, bad := range map[string][]byte{
		"oversize":         bytes.Repeat([]byte("x"), MaxBytes+1),
		"UTF8":             append(bytes.Clone(wire), 0xff),
		"unknown":          bytes.Replace(wire, []byte(`"version":1`), []byte(`"extra":1,"version":1`), 1),
		"duplicate":        bytes.Replace(wire, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"nested duplicate": bytes.Replace(wire, []byte(`"mode":"ephemeral"`), []byte(`"mode":"ephemeral","mode":"ephemeral"`), 1),
		"nested unknown":   bytes.Replace(wire, []byte(`"mode":"ephemeral"`), []byte(`"mode":"ephemeral","other":0`), 1),
		"version":          bytes.Replace(wire, []byte(`"version":1`), []byte(`"version":2`), 1),
		"space":            append([]byte(" "), wire...),
		"trailing":         append(bytes.Clone(wire), []byte("{}")...),
		"null":             []byte("null"),
		"escaped name":     bytes.Replace(wire, []byte(`"mode"`), []byte(`"\u006dode"`), 1),
		"digest":           bytes.Replace(wire, []byte(snapshot.RequestedDigest), []byte(strings.Repeat("a", 64)), 1),
	} {
		t.Run(name, func(t *testing.T) { _, err := Decode(bad); require.Error(t, err) })
	}
	for name, mutate := range map[string]func(*Snapshot){
		"version":           func(s *Snapshot) { s.Version++ },
		"digest":            func(s *Snapshot) { s.RequestedDigest = strings.Repeat("a", 64) },
		"huge digest":       func(s *Snapshot) { s.RequestedDigest = strings.Repeat("a", 4*MaxBytes) },
		"request":           func(s *Snapshot) { s.Requested.Mode = "persistent" },
		"resolved zero":     func(s *Snapshot) { s.Resolved.Resources.CPU = "0" },
		"resolved huge":     func(s *Snapshot) { s.Resolved.Resources.CPU = strings.Repeat("9", MaxBytes+1) },
		"ttl kind":          func(s *Snapshot) { s.Resolved.TTL.Kind = "forever" },
		"huge ttl kind":     func(s *Snapshot) { s.Resolved.TTL.Kind = strings.Repeat("a", 4*MaxBytes) },
		"ttl zero":          func(s *Snapshot) { s.Resolved.TTL.Seconds = 0 },
		"permanent seconds": func(s *Snapshot) { s.Resolved.TTL = TTL{Kind: "permanent", Seconds: 1} },
		"mount":             func(s *Snapshot) { s.Resolved.WorkspaceMountMode = types.WorkspaceMountSync },
		"unnormalized":      func(s *Snapshot) { s.Requested.Network = &types.NetworkConfig{} },
	} {
		t.Run(name, func(t *testing.T) {
			forged := *snapshot
			mutate(&forged)
			_, err := Encode(&forged)
			require.Error(t, err)
			bad, err := json.Marshal(&forged)
			require.NoError(t, err)
			_, err = Decode(bad)
			require.Error(t, err)
		})
	}
	_, err = Encode(nil)
	require.Error(t, err)
}

func TestBusinessSnapshotExplicitResolvedConsistency(t *testing.T) {
	request := types.CreateSandboxRequest{Mode: "persistent", Timeout: 60, Resources: &types.ResourceLimits{CPU: "2000m", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}, WorkspacePath: "w", WorkspaceMountMode: types.WorkspaceMountSync}
	snapshot, err := Build(request, snapshotDefaults())
	require.NoError(t, err)
	for name, mutate := range map[string]func(*Snapshot){
		"timeout":   func(s *Snapshot) { s.Resolved.TTL.Seconds++ },
		"permanent": func(s *Snapshot) { s.Resolved.TTL = TTL{Kind: "permanent"} },
		"CPU":       func(s *Snapshot) { s.Resolved.Resources.CPU = "3" },
		"memory":    func(s *Snapshot) { s.Resolved.Resources.Memory = "1Gi" },
		"disk":      func(s *Snapshot) { s.Resolved.Resources.Disk = "2Mi" },
		"tmp":       func(s *Snapshot) { s.Resolved.Resources.TmpDisk = "51Mi" },
		"mount":     func(s *Snapshot) { s.Resolved.WorkspaceMountMode = types.WorkspaceMountFUSE },
	} {
		t.Run(name, func(t *testing.T) {
			forged := *snapshot
			mutate(&forged)
			_, err := Encode(&forged)
			require.Error(t, err)
			bad, err := json.Marshal(&forged)
			require.NoError(t, err)
			_, err = Decode(bad)
			require.Error(t, err)
		})
	}
	snapshot.Resolved.Resources.CPU = "2"
	_, err = Encode(snapshot)
	require.NoError(t, err, "equivalent Kubernetes quantities remain consistent")
}
