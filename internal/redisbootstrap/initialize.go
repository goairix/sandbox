package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type InitializeOptions struct {
	Namespace, Name                            string
	PublicKeys                                 [3]ed25519.PublicKey
	MasterName, DataPassword, SentinelPassword string
	AckTimeout, Timeout                        time.Duration
}

type initializeDependencies struct {
	topology topologyDependencies
}

var errInitialize = errors.New("redis bootstrap initialization is unconfirmed")
var errInitializeIdentity = errors.New("retained bootstrap installation identity changed")

func InitializeNamespaceBootstrap(ctx context.Context, client corev1.ConfigMapInterface, o InitializeOptions) error {
	return initializeNamespaceBootstrap(ctx, client, o, initializeDependencies{topology: topologyDependencies{dial: (&net.Dialer{Timeout: time.Second}).DialContext, fetch: FetchIdentityProofContact}})
}

// InitializeNamespaceBootstrap grants only an existing namespace identity. The
// client must be scoped to Namespace and authorized for get/update on Name only.
// It does not create/delete objects, proxy writes or replace Sentinel elections.
func initializeNamespaceBootstrap(ctx context.Context, client corev1.ConfigMapInterface, o InitializeOptions, d initializeDependencies) (resultErr error) {
	if ctx == nil || client == nil || !validInitializeOptions(o) || d.topology.dial == nil || d.topology.fetch == nil {
		return errInitialize
	}
	caller := ctx
	defer func() {
		if resultErr != nil {
			if caller.Err() != nil {
				resultErr = caller.Err()
			} else {
				resultErr = errInitialize
			}
		}
	}()
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 12 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var pinned *NamespaceBootstrap
	for {
		state, err := LoadNamespaceBootstrap(ctx, client, o.Namespace, o.Name)
		if err != nil {
			var apiError namespaceBootstrapAPIError
			if !errors.As(err, &apiError) || (pinned != nil && apierrors.IsNotFound(err)) {
				return errInitialize
			}
		} else {
			if pinned != nil && !sameBootstrapInstallation(*pinned, state) {
				return errInitializeIdentity
			}
			copy := state
			if state.Registration != nil {
				r := *state.Registration
				copy.Registration = &r
			}
			pinned = &copy
			if state.Registration == nil {
				challenges, proofs, err := readFreshRegistrationInventory(ctx, state.Cluster, o.PublicKeys, d.topology)
				if err == nil {
					registered, err := RegisterNamespaceBootstrap(ctx, client, o.Namespace, o.Name, state.UID, state.ResourceVersion, o.PublicKeys, challenges, proofs)
					if err == nil {
						if !sameBootstrapInstallation(state, registered) || registered.Registration == nil {
							return errInitializeIdentity
						}
						copy := registered
						r := *registered.Registration
						copy.Registration = &r
						pinned = &copy
						continue // Re-read the server grant before using it.
					}
				}
			} else {
				digest, err := PublicKeySetDigest(o.PublicKeys)
				if err != nil || digest != state.Registration.KeyDigest {
					return errInitializeIdentity
				}
				topology := TopologyOptions{Registration: *state.Registration, PublicKeys: o.PublicKeys, MasterName: o.MasterName, DataPassword: o.DataPassword, SentinelPassword: o.SentinelPassword, AckTimeout: o.AckTimeout}
				if err := verifyTopology(ctx, topology, state.Cluster.Phase == Pending, d.topology); err == nil {
					if state.Cluster.Phase == Initialized {
						current, err := LoadNamespaceBootstrap(ctx, client, o.Namespace, o.Name)
						if err == nil {
							if !sameBootstrapInstallation(state, current) {
								return errInitializeIdentity
							}
							if current.ResourceVersion == state.ResourceVersion {
								return ctx.Err()
							}
						}
					} else {
						_, err := commitInitializedBootstrap(ctx, client, o.Namespace, o.Name, state)
						if err == nil {
							return ctx.Err()
						}
						if errors.Is(err, errInitializeIdentity) {
							return err
						}
					}
				}
			}
		}
		if err := waitBootstrapPoll(ctx); err != nil {
			return err
		}
	}
}

func validInitializeOptions(o InitializeOptions) bool {
	_, err := PublicKeySetDigest(o.PublicKeys)
	return len(o.Namespace) <= 63 && dnsLabelPattern.MatchString(o.Namespace) && validDNS(o.Name) && err == nil && safeMasterName(o.MasterName) && ValidateBuiltinSentinelPassword(o.DataPassword) == nil && ValidateBuiltinSentinelPassword(o.SentinelPassword) == nil && o.DataPassword != o.SentinelPassword && o.AckTimeout >= time.Second && o.AckTimeout <= 10*time.Second && o.Timeout >= 0 && o.Timeout <= 12*time.Minute
}

func sameBootstrapInstallation(before, after NamespaceBootstrap) bool {
	if before.UID != after.UID || before.Cluster.ClusterID != after.Cluster.ClusterID || before.Cluster.Members != after.Cluster.Members || (before.Cluster.Phase == Initialized && after.Cluster.Phase != Initialized) {
		return false
	}
	if before.Registration == nil {
		return true
	}
	return after.Registration != nil && before.Registration.KeyDigest == after.Registration.KeyDigest && before.Registration.MarkerIDs == after.Registration.MarkerIDs
}

func readFreshRegistrationInventory(ctx context.Context, c ClusterState, keys [3]ed25519.PublicKey, d topologyDependencies) ([3]IdentityChallenge, [3]IdentityProof, error) {
	var challenges [3]IdentityChallenge
	var proofs [3]IdentityProof
	type result struct {
		ordinal   int
		challenge IdentityChallenge
		proof     IdentityProof
		err       error
	}
	replies := make(chan result, 3)
	for i := range 3 {
		go func(i int) {
			ch, err := NewIdentityChallenge(InventoryProof)
			if err != nil {
				replies <- result{err: err}
				return
			}
			member := fixedTopologyMember(c, i)
			p, state, err := d.fetch(ctx, c, member, keys[i], ch, nil, nil)
			if err == nil && (state != ContactVerified || VerifyIdentityProof(keys[i], c, member, ch, p, nil, nil) != nil) {
				err = errInitialize
			}
			replies <- result{i, ch, p, err}
		}(i)
	}
	var failure error
	for range 3 {
		r := <-replies
		if r.err != nil {
			failure = errInitialize
		}
		challenges[r.ordinal] = r.challenge
		proofs[r.ordinal] = r.proof
	}
	if failure != nil || ctx.Err() != nil {
		return [3]IdentityChallenge{}, [3]IdentityProof{}, errInitialize
	}
	if _, err := RegisterFreshVolumes(c, keys, challenges, proofs); err != nil {
		return [3]IdentityChallenge{}, [3]IdentityProof{}, errInitialize
	}
	return challenges, proofs, nil
}

// This private final CAS is called only after current topology and new-write
// ACK verification. A conflict forces the outer loop to reverify that whole
// flow; resourceVersion is never borrowed from a later unverified GET.
func commitInitializedBootstrap(ctx context.Context, client corev1.ConfigMapInterface, namespace, name string, expected NamespaceBootstrap) (NamespaceBootstrap, error) {
	cm, current, err := getNamespaceBootstrap(ctx, client, namespace, name)
	if err != nil {
		return NamespaceBootstrap{}, err
	}
	if !sameBootstrapInstallation(expected, current) {
		return NamespaceBootstrap{}, errInitializeIdentity
	}
	if current.ResourceVersion != expected.ResourceVersion {
		return NamespaceBootstrap{}, namespaceBootstrapAPIError{cause: apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, errors.New("bootstrap version changed"))}
	}
	if expected.Cluster.Phase != Pending || expected.Registration == nil || current.Registration == nil || *expected.Registration != *current.Registration {
		return NamespaceBootstrap{}, errInitializeIdentity
	}
	initialized := expected.Cluster
	initialized.Phase = Initialized
	registration := *expected.Registration
	registration.Cluster = initialized
	clusterJSON, _ := json.Marshal(initialized)
	registrationJSON, _ := json.Marshal(registration)
	update := cm.DeepCopy()
	update.Data["cluster.json"] = string(clusterJSON)
	update.Data["registration.json"] = string(registrationJSON)
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
	if err != nil || accepted.UID != expected.UID || accepted.Cluster != initialized || accepted.Registration == nil || *accepted.Registration != registration {
		return NamespaceBootstrap{}, errInitializeIdentity
	}
	return accepted, nil
}
