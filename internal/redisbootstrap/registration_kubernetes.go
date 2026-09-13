package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"

	api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// NamespaceBootstrap is a validated read of the namespace's pinned installation identity.
type NamespaceBootstrap struct {
	Cluster         ClusterState
	Registration    *BootstrapRegistration
	UID             types.UID
	ResourceVersion string
}

// LoadNamespaceBootstrap reads only the named existing ConfigMap. The caller must
// supply a client scoped to namespace; returned namespace/name, UID and version
// are checked. Missing or malformed objects fail closed and are never created.
func LoadNamespaceBootstrap(ctx context.Context, client corev1.ConfigMapInterface, namespace, name string) (NamespaceBootstrap, error) {
	_, state, err := getNamespaceBootstrap(ctx, client, namespace, name)
	return state, err
}

// namespaceBootstrapAPIError keeps API reason typing through Unwrap without
// displaying server errors, which can include rejected ConfigMap payloads.
type namespaceBootstrapAPIError struct{ cause error }

func (e namespaceBootstrapAPIError) Error() string {
	return "namespace bootstrap ConfigMap API request failed"
}
func (e namespaceBootstrapAPIError) Unwrap() error { return e.cause }

func parseNamespaceBootstrap(cm *api.ConfigMap, namespace, name string) (NamespaceBootstrap, error) {
	if cm == nil || namespace == "" || name == "" || cm.Namespace != namespace || cm.Name != name || cm.UID == "" || cm.ResourceVersion == "" || len(cm.BinaryData) != 0 {
		return NamespaceBootstrap{}, errBootstrapRegistration
	}
	clusterJSON, ok := cm.Data["cluster.json"]
	if !ok {
		return NamespaceBootstrap{}, errBootstrapRegistration
	}
	for key := range cm.Data {
		if key != "cluster.json" && key != "registration.json" {
			return NamespaceBootstrap{}, errBootstrapRegistration
		}
	}
	cluster, err := ParseClusterState([]byte(clusterJSON))
	if err != nil {
		return NamespaceBootstrap{}, errBootstrapRegistration
	}
	state := NamespaceBootstrap{Cluster: cluster, UID: cm.UID, ResourceVersion: cm.ResourceVersion}
	if data, present := cm.Data["registration.json"]; present {
		registration, err := ParseBootstrapRegistration([]byte(data))
		if err != nil || registration.Cluster != cluster {
			return NamespaceBootstrap{}, errBootstrapRegistration
		}
		state.Registration = &registration
	} else if cluster.Phase == Initialized {
		return NamespaceBootstrap{}, errBootstrapRegistration
	}
	return state, nil
}

func getNamespaceBootstrap(ctx context.Context, client corev1.ConfigMapInterface, namespace, name string) (*api.ConfigMap, NamespaceBootstrap, error) {
	if err := ctx.Err(); err != nil {
		return nil, NamespaceBootstrap{}, err
	}
	if client == nil || namespace == "" || name == "" {
		return nil, NamespaceBootstrap{}, errBootstrapRegistration
	}
	cm, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, NamespaceBootstrap{}, namespaceBootstrapAPIError{cause: err}
	}
	if err := ctx.Err(); err != nil {
		return nil, NamespaceBootstrap{}, err
	}
	state, err := parseNamespaceBootstrap(cm, namespace, name)
	return cm, state, err
}

// RegisterNamespaceBootstrap performs one pinned GET/UPDATE CAS with a client
// scoped to namespace. expectedUID/expectedRV must be obtained from a prior trusted
// read. Three distinct caller-generated one-use inventory challenges are required.
// Existing valid registration is verified but never updated or reset. A new
// registration keeps Pending; conflicts are returned without retries. This does
// not create objects, initialize topology, elect roles or open the business gate.
func RegisterNamespaceBootstrap(ctx context.Context, client corev1.ConfigMapInterface, namespace, name string, expectedUID types.UID, expectedRV string, keys [3]ed25519.PublicKey, challenges [3]IdentityChallenge, proofs [3]IdentityProof) (NamespaceBootstrap, error) {
	if err := ctx.Err(); err != nil {
		return NamespaceBootstrap{}, err
	}
	if expectedUID == "" || expectedRV == "" {
		return NamespaceBootstrap{}, errBootstrapRegistration
	}
	cm, state, err := getNamespaceBootstrap(ctx, client, namespace, name)
	if err != nil {
		return NamespaceBootstrap{}, err
	}
	if state.UID != expectedUID || state.ResourceVersion != expectedRV {
		return NamespaceBootstrap{}, namespaceBootstrapAPIError{cause: apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, errors.New("installation identity or version changed"))}
	}
	if registrationInventoryChallenges(challenges) != nil {
		return NamespaceBootstrap{}, errBootstrapRegistration
	}
	if state.Registration != nil {
		for i, proof := range proofs {
			if proof.Member != (Member{DNS: state.Cluster.Members[i], Ordinal: i}) || VerifyRegisteredProof(*state.Registration, keys, challenges[i], proof, nil) != nil {
				return NamespaceBootstrap{}, errBootstrapRegistration
			}
		}
		return state, nil
	}
	registration, err := RegisterFreshVolumes(state.Cluster, keys, challenges, proofs)
	if err != nil {
		return NamespaceBootstrap{}, err
	}
	// Concrete registration contains no custom marshaler or non-JSON values.
	data, _ := json.Marshal(registration)
	update := cm.DeepCopy()
	update.Data["registration.json"] = string(data)
	if err := ctx.Err(); err != nil {
		return NamespaceBootstrap{}, err
	}
	updated, err := client.Update(ctx, update, metav1.UpdateOptions{})
	if err != nil {
		return NamespaceBootstrap{}, namespaceBootstrapAPIError{cause: err}
	}
	if err := ctx.Err(); err != nil {
		return NamespaceBootstrap{}, err
	}
	accepted, err := parseNamespaceBootstrap(updated, namespace, name)
	if err != nil || accepted.UID != expectedUID || accepted.Cluster != state.Cluster || accepted.Registration == nil || *accepted.Registration != registration {
		return NamespaceBootstrap{}, errBootstrapRegistration
	}
	return accepted, nil
}
