package sandboxsnapshot

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goairix/sandbox/pkg/types"
	"github.com/stretchr/testify/require"
)

// A short exponent must not authorize work proportional to its numeric value.
// This positive, same-scale fixture is safe on the original parser: neither
// Sign nor comparing identical scales expands the decimal representation.
func TestBusinessSnapshotQuantityExponentBudget(t *testing.T) {
	defaults := snapshotDefaults()
	defaults.Resources.CPU = "1e61441"
	_, err := Build(types.CreateSandboxRequest{Mode: "ephemeral", Resources: &types.ResourceLimits{CPU: "1e61441"}}, defaults)
	require.Error(t, err, "decimal exponent exceeds the canonical wire byte budget before quantity parsing")
}

func TestBusinessSnapshotQuantityExponentLexicalBounds(t *testing.T) {
	for _, value := range []string{"2", "512Mi", "1E", "1Ei", "1e61440", "1E+61440", "1e-61440", "1e000061440"} {
		require.True(t, boundedQuantityExponent(value), value)
	}
	for _, value := range []string{"1e61441", "1E+61441", "1e-61441", "1e2147483647", "1e-2147483647", "1e9223372036854775808", "1e" + strings.Repeat("9", 100), "1e+", "1e--1"} {
		require.False(t, boundedQuantityExponent(value), value)
	}
	for _, value := range []string{"1E", "1Ei", "1e61440"} {
		defaults := snapshotDefaults()
		defaults.Resources.CPU = value
		snapshot, err := Build(types.CreateSandboxRequest{Mode: "ephemeral", Resources: &types.ResourceLimits{CPU: value}}, defaults)
		require.NoError(t, err, value)
		wire, err := Encode(snapshot)
		require.NoError(t, err)
		_, err = Decode(wire)
		require.NoError(t, err)
	}
}

func TestBusinessSnapshotQuantityExponentEveryResourceAndCodec(t *testing.T) {
	for _, name := range []string{"CPU", "Memory", "Disk", "TmpDisk"} {
		for _, requested := range []bool{false, true} {
			for _, value := range []string{"1e61441", "1e-61441", "1e9223372036854775808"} {
				t.Run(name+"/"+value+"/"+map[bool]string{true: "requested", false: "resolved"}[requested], func(t *testing.T) {
					defaults := snapshotDefaults()
					request := types.CreateSandboxRequest{Mode: "ephemeral"}
					set := func(limits *types.ResourceLimits) {
						switch name {
						case "CPU":
							limits.CPU = value
						case "Memory":
							limits.Memory = value
						case "Disk":
							limits.Disk = value
						case "TmpDisk":
							limits.TmpDisk = value
						}
					}
					if requested {
						request.Resources = &types.ResourceLimits{}
						set(request.Resources)
					} else {
						set(&defaults.Resources)
					}
					_, err := Build(request, defaults)
					require.Error(t, err)
					// Build is bypassed deliberately to exercise Encode/Decode's
					// independent trust boundary with an otherwise valid envelope.
					valid, err := Build(types.CreateSandboxRequest{Mode: "ephemeral"}, snapshotDefaults())
					require.NoError(t, err)
					if requested {
						valid.Requested.Resources = &types.ResourceLimits{}
						set(valid.Requested.Resources)
					} else {
						set(&valid.Resolved.Resources)
					}
					valid.RequestedDigest = requestedDigest(valid.Requested)
					_, err = Encode(valid)
					require.Error(t, err)
					wire, err := json.Marshal(valid)
					require.NoError(t, err)
					_, err = Decode(wire)
					require.Error(t, err)
				})
			}
		}
	}
}
