package kubernetes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestTaskRuntimeCleanupClientPolicyUID(t *testing.T) {
	valid := "abcdefab-cdef-4abc-8def-abcdefabcdef"
	invalid := []struct{ name, uid string }{
		{"malformed", "not-a-uuid"},
		{"nil", "00000000-0000-0000-0000-000000000000"},
		{"noncanonical", strings.ToUpper(valid)},
	}
	for _, kind := range []string{"NetworkPolicy", "CiliumNetworkPolicy"} {
		prefix, step := "/apis/networking.k8s.io/v1/namespaces/runtimes/networkpolicies/", TaskRuntimeCleanupDeleteNetworkPolicyLogical
		name := "sandbox-logical-a"
		if kind == "CiliumNetworkPolicy" {
			prefix, step = "/apis/cilium.io/v2/namespaces/runtimes/ciliumnetworkpolicies/", TaskRuntimeCleanupDeleteCiliumPolicyLogical
			name = "sandbox-private-deny-logical-a"
		}
		for _, source := range []string{"live", "saved"} {
			for _, bad := range invalid {
				t.Run(kind+"/"+source+"/"+bad.name, func(t *testing.T) {
					f, options, binding := newCleanupAPIFixture(t, false)
					client, err := NewTaskRuntimeCleanupClient(options)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, client.Close()) })
					var wire []byte
					if source == "saved" {
						h, err := client.Prepare(context.Background(), binding, nil)
						require.NoError(t, err)
						var descriptor taskCleanupDescriptor
						require.NoError(t, json.Unmarshal(h.Descriptor(), &descriptor))
						matched := false
						for i := range descriptor.Targets {
							if descriptor.Targets[i].Kind == kind && descriptor.Targets[i].Name == name {
								descriptor.Targets[i].UID = bad.uid
								matched = true
							}
						}
						require.True(t, matched)
						wire, err = json.Marshal(descriptor)
						require.NoError(t, err)
						// Absence must not hide an invalid authenticated descriptor
						// identity: this forces validation at the saved boundary.
						f.mu.Lock()
						delete(f.policies, prefix+name)
						f.mu.Unlock()
					} else {
						f.mu.Lock()
						f.policies[prefix+name].SetUID(types.UID(bad.uid))
						f.mu.Unlock()
					}
					_, err = client.Prepare(context.Background(), binding, wire)
					require.Empty(t, f.receipts(), "rejected live/saved identities send zero mutations")
					t.Logf("actual received mutation requests=%d", len(f.receipts()))
					require.Error(t, err, "policy object UID must be a canonical nonzero UUID")
				})
			}
		}
		for _, absent := range []bool{false, true} {
			t.Run(kind+"/valid-"+map[bool]string{false: "uuid", true: "absent"}[absent], func(t *testing.T) {
				f, options, binding := newCleanupAPIFixture(t, false)
				f.pod.Finalizers = []string{taskCleanupFinalizer}
				now := metav1.Now()
				f.pod.DeletionTimestamp = &now
				if absent {
					delete(f.policies, prefix+name)
				} else {
					f.policies[prefix+name].SetUID(types.UID(valid))
				}
				client, err := NewTaskRuntimeCleanupClient(options)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				h, err := client.Prepare(context.Background(), binding, nil)
				require.NoError(t, err)
				restored, err := client.Prepare(context.Background(), binding, h.Descriptor())
				require.NoError(t, err, "canonical UUID and empty UID/RV pair survive saved roundtrip")
				snapshot, err := restored.ObserveStep(context.Background(), step)
				require.NoError(t, err)
				require.Equal(t, absent, snapshot.Absent)
				attempt, err := restored.Apply(context.Background(), step)
				require.NoError(t, err)
				require.Equal(t, snapshot, attempt)
				if absent {
					require.Empty(t, f.receipts())
					require.Empty(t, snapshot.UID)
					require.Empty(t, snapshot.ResourceVersion)
				} else {
					require.Len(t, f.receipts(), 1)
					var body metav1.DeleteOptions
					require.NoError(t, json.Unmarshal(f.receipts()[0].body, &body))
					require.Equal(t, types.UID(valid), *body.Preconditions.UID)
				}
				t.Logf("actual received mutation requests=%d", len(f.receipts()))
			})
		}
	}
}
