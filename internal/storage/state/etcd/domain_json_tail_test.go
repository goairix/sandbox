package etcd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// These tests exercise the real metadata decoders. A second JSON value must
// never become a generic object graph merely to establish that trailing input
// is forbidden; the allocation check is not a whole-worker memory benchmark.
func TestDomainJSONTailRejection(t *testing.T) {
	original := WorkspaceFenceRecord{Version: 1, WorkspaceHash: domainHash, RestoreEpoch: "restore", Generation: 1}
	wire, err := json.Marshal(original)
	require.NoError(t, err)
	locator := StageAttemptLocator{
		Namespace: "/sandbox/", RestoreEpoch: "restore", Partition: 1,
		RequestID: "request", StageID: "runtime_dispatch", AttemptID: "11111111-1111-4111-8111-111111111111",
	}
	reference := locator.reference(domainHash)
	receipt, err := encodeReceipt(reference, OutcomeCommitted)
	require.NoError(t, err)

	for _, suffix := range []string{"", " \t\r\n", " {}", " []", " null", " true", " 42", ` "second"`, " {", " [", " !", " ]", " /*comment*/"} {
		t.Run("tail="+suffix, func(t *testing.T) {
			valid := strings.TrimSpace(suffix) == ""
			value := append(append([]byte(nil), wire...), suffix...)
			metadataErr := strictPreparationMetadata(value, reflect.TypeOf(original))
			got := original
			got.Generation = 7
			before := got
			domainErr := decodeTypedDomain(&mvccpb.KeyValue{Value: value}, &got, maxRecordBytes)
			ref, receiptErr := decodeRuntimeDispatchReceipt(&mvccpb.KeyValue{
				CreateRevision: 1, ModRevision: 1, Version: 1, Value: []byte(receipt + suffix),
			}, locator)
			if valid {
				require.NoError(t, metadataErr)
				require.NoError(t, domainErr)
				require.Equal(t, original, got)
				require.NoError(t, receiptErr)
				require.Equal(t, reference, ref)
				return
			}

			require.ErrorIs(t, metadataErr, ErrCorruptRecord)
			require.ErrorIs(t, domainErr, ErrCorruptRecord)
			require.Equal(t, before, got, "refusing trailing input must not install a partial destination")
			require.ErrorIs(t, receiptErr, ErrCorruptReceipt)
			require.Equal(t, StageReference{}, ref)
		})
	}
}

func TestDomainJSONTailDoesNotExpandSecondValue(t *testing.T) {
	original := WorkspaceFenceRecord{Version: 1, WorkspaceHash: domainHash, RestoreEpoch: "restore", Generation: 1}
	wire, err := json.Marshal(original)
	require.NoError(t, err)
	// A fixed 16KiB second array is within the generic decoder's actual 64KiB
	// input cap. Decoding it into any used thousands of map/slice allocations;
	// rejecting its first token should remain independent of its element count.
	tail := " [" + strings.Repeat(`{"x":[0,1,2]},`, 1024) + "{}]"
	value := append(append([]byte(nil), wire...), tail...)
	require.Less(t, len(value), maxRecordBytes)
	for name, decode := range map[string]func() error{
		"strict-metadata": func() error { return strictPreparationMetadata(value, reflect.TypeOf(original)) },
		"typed-domain": func() error {
			got := original
			return decodeTypedDomain(&mvccpb.KeyValue{Value: value}, &got, maxRecordBytes)
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, decode(), ErrCorruptRecord)
			var last error
			allocations := testing.AllocsPerRun(3, func() { last = decode() })
			require.ErrorIs(t, last, ErrCorruptRecord)
			// This deliberately loose ceiling covers the legitimate first
			// record's typed validation on the repository's Go toolchain. It
			// distinguishes expanding 1024 objects from reading one token.
			require.Less(t, allocations, float64(256))
			t.Logf("reject second 1024-object value: %.0f allocations", allocations)
		})
	}
}
