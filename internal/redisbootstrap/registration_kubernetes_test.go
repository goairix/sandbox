package redisbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	kt "k8s.io/client-go/testing"
)

func registrationConfigMap(t *testing.T, f registrationFixture, r *BootstrapRegistration) *api.ConfigMap {
	t.Helper()
	cluster, err := json.Marshal(f.cluster)
	if err != nil {
		t.Fatal(err)
	}
	cm := &api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "isolated", Name: "identity", UID: "expected-uid", ResourceVersion: "7", Labels: map[string]string{"release": "preserve"}, Annotations: map[string]string{"existing": "preserve"}, Finalizers: []string{"example.test/preserve"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: "owner", UID: "owner-uid"}}}, Data: map[string]string{"cluster.json": string(cluster)}}
	if r != nil {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		cm.Data["registration.json"] = string(data)
	}
	return cm
}

func assertRegistrationActions(t *testing.T, client *fake.Clientset, updates int) {
	t.Helper()
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" {
			count++
		}
		if action.GetResource().Resource != "configmaps" || (action.GetVerb() != "get" && action.GetVerb() != "update") {
			t.Fatalf("unexpected mutation/action: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
	if count != updates {
		t.Fatalf("updates = %d, want %d", count, updates)
	}
}

func TestLoadNamespaceBootstrapValidPhases(t *testing.T) {
	for _, phase := range []Phase{Pending, Initialized} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRegistrationFixture(t)
			f.cluster.Phase = phase
			r := fixtureRegistration(f)
			var registration *BootstrapRegistration
			if phase == Initialized {
				registration = &r
			}
			client := fake.NewClientset(registrationConfigMap(t, f, registration))
			got, err := LoadNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity")
			if err != nil || got.Cluster != f.cluster || got.UID != "expected-uid" || got.ResourceVersion != "7" {
				t.Fatal("valid scoped state rejected:", err)
			}
			if phase == Initialized && (got.Registration == nil || *got.Registration != r) {
				t.Fatal("lost initialized registration")
			}
			if phase == Pending && got.Registration != nil {
				t.Fatal("invented registration")
			}
			assertRegistrationActions(t, client, 0)
		})
	}
}

func TestLoadNamespaceBootstrapRejectsInvalidObjects(t *testing.T) {
	for _, name := range []string{"wrong namespace", "wrong name", "missing UID", "missing RV", "missing cluster", "unknown data", "binary data", "invalid cluster", "cluster JSON secret", "initialized unregistered", "null registration", "foreign registration", "phase mismatch", "members mismatch"} {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t)
			r := fixtureRegistration(f)
			cm := registrationConfigMap(t, f, nil)
			switch name {
			case "wrong namespace":
				cm.Namespace = "other"
			case "wrong name":
				cm.Name = "other"
			case "missing UID":
				cm.UID = ""
			case "missing RV":
				cm.ResourceVersion = ""
			case "missing cluster":
				delete(cm.Data, "cluster.json")
			case "unknown data":
				cm.Data["foreign.json"] = "PRIVATE-CM-CONTENT"
			case "binary data":
				cm.BinaryData = map[string][]byte{"foreign": []byte("PRIVATE-CM-CONTENT")}
			case "invalid cluster":
				cm.Data["cluster.json"] = "null"
			case "cluster JSON secret":
				cm.Data["cluster.json"] = `{"PRIVATE-CM-CONTENT":"secret"}`
			case "initialized unregistered":
				f.cluster.Phase = Initialized
				cm.Data["cluster.json"] = mustRegistrationJSON(t, f.cluster)
			case "null registration":
				cm.Data["registration.json"] = "null"
			case "foreign registration":
				r.Cluster.ClusterID = "foreign"
				cm.Data["registration.json"] = mustRegistrationJSON(t, r)
			case "phase mismatch":
				r.Cluster.Phase = Initialized
				cm.Data["registration.json"] = mustRegistrationJSON(t, r)
			case "members mismatch":
				r.Cluster.Members[0] = "other.isolated.svc"
				cm.Data["registration.json"] = mustRegistrationJSON(t, r)
			}
			client := fake.NewClientset()
			client.PrependReactor("get", "configmaps", func(kt.Action) (bool, runtime.Object, error) { return true, cm, nil })
			if _, err := LoadNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity"); err == nil {
				t.Fatal("invalid object accepted")
			} else if strings.Contains(err.Error(), "PRIVATE-CM-CONTENT") {
				t.Fatal("CM leaked in error")
			}
			assertRegistrationActions(t, client, 0)
		})
	}
}

func TestRegisterNamespaceBootstrapExistingRegistrationNeverUpdates(t *testing.T) {
	for _, phase := range []Phase{Pending, Initialized} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRegistrationFixture(t)
			r := fixtureRegistration(f)
			f.cluster.Phase = phase
			r.Cluster = f.cluster
			for i := range f.proofs {
				f.challenges[i], _ = NewIdentityChallenge(InventoryProof)
				observation := f.proofs[i].Observation
				if phase == Initialized || i == 1 {
					observation = configuredRegistrationObservation(f, i, InventoryProof)
				}
				f.proofs[i] = f.sign(t, i, f.challenges[i], observation)
			}
			original := registrationConfigMap(t, f, &r)
			client := fake.NewClientset(original)
			for job := 0; job < 2; job++ {
				got, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs)
				if err != nil || got.Registration == nil || *got.Registration != r || got.Cluster.Phase != phase {
					t.Fatal("existing registration rejected/reset:", err)
				}
				for i := range f.proofs {
					f.challenges[i], _ = NewIdentityChallenge(InventoryProof)
					f.proofs[i] = f.sign(t, i, f.challenges[i], f.proofs[i].Observation)
				}
			}
			assertRegistrationActions(t, client, 0)
		})
	}
}

func TestRegisterNamespaceBootstrapPinsGetIdentity(t *testing.T) {
	for _, name := range []string{"replacement UID", "changed RV", "empty expected UID", "empty expected RV"} {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t)
			cm := registrationConfigMap(t, f, nil)
			uid := types.UID("expected-uid")
			rv := "7"
			switch name {
			case "replacement UID":
				cm.UID = "replacement"
			case "changed RV":
				cm.ResourceVersion = "8"
			case "empty expected UID":
				uid = ""
			case "empty expected RV":
				rv = ""
			}
			client := fake.NewClientset(cm)
			if _, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", uid, rv, f.keys, f.challenges, f.proofs); err == nil {
				t.Fatal("unpinned object accepted")
			}
			assertRegistrationActions(t, client, 0)
			if (uid == "" || rv == "") && len(client.Actions()) != 0 {
				t.Fatal("invalid pin contacted API")
			}
		})
	}
}

func TestRegisterNamespaceBootstrapRejectsExistingIdentityChanges(t *testing.T) {
	for _, name := range []string{"key", "marker", "empty", "Reserved Initialized", "same nonce", "swapped", "live"} {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t)
			r := fixtureRegistration(f)
			switch name {
			case "key":
				f.keys[1] = f.keys[0]
			case "marker":
				identity := *f.proofs[1].Observation.Volume.Identity
				identity.MarkerID = strings.Repeat("e", 32)
				f.proofs[1] = f.sign(t, 1, f.challenges[1], LocalObservation{Volume: VolumeState{Identity: &identity}})
			case "empty":
				f.proofs[1] = f.sign(t, 1, f.challenges[1], LocalObservation{Volume: VolumeState{Empty: true}})
			case "Reserved Initialized":
				f.cluster.Phase = Initialized
				r.Cluster = f.cluster
			case "same nonce":
				f.challenges[1] = f.challenges[0]
				f.proofs[1] = f.sign(t, 1, f.challenges[1], f.proofs[1].Observation)
			case "swapped":
				f.proofs[0], f.proofs[1] = f.proofs[1], f.proofs[0]
				f.challenges[0], f.challenges[1] = f.challenges[1], f.challenges[0]
			case "live":
				f.challenges[1], _ = NewIdentityChallenge(LiveProof)
				f.proofs[1] = f.sign(t, 1, f.challenges[1], configuredRegistrationObservation(f, 1, LiveProof))
			}
			client := fake.NewClientset(registrationConfigMap(t, f, &r))
			if _, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs); err == nil {
				t.Fatal("existing identity reset/changed")
			}
			assertRegistrationActions(t, client, 0)
		})
	}
}

func TestNamespaceBootstrapMissingDoesNotCreate(t *testing.T) {
	f := newRegistrationFixture(t)
	client := fake.NewClientset()
	if _, err := LoadNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity"); !apierrors.IsNotFound(err) {
		t.Fatal("lost NotFound type:", err)
	}
	if _, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs); !apierrors.IsNotFound(err) {
		t.Fatal("lost register NotFound type:", err)
	}
	assertRegistrationActions(t, client, 0)
}

func TestRegisterNamespaceBootstrapConflictNoRetry(t *testing.T) {
	f := newRegistrationFixture(t)
	client := fake.NewClientset(registrationConfigMap(t, f, nil))
	updates := 0
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "identity", errors.New("PRIVATE-CM-CONTENT"))
	client.PrependReactor("update", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
		updates++
		cm := action.(kt.UpdateAction).GetObject().(*api.ConfigMap)
		if cm.UID != "expected-uid" || cm.ResourceVersion != "7" {
			t.Fatal("CAS not pinned")
		}
		return true, nil, conflict
	})
	_, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs)
	if !apierrors.IsConflict(err) || !errors.Is(err, conflict) || strings.Contains(err.Error(), "PRIVATE-CM-CONTENT") {
		t.Fatal("lost/redaction of conflict failed:", err)
	}
	if updates != 1 {
		t.Fatal("conflict retried")
	}
	assertRegistrationActions(t, client, 1)
	if len(client.Actions()) != 2 {
		t.Fatal("conflict used extra GET/retry")
	}
}

func TestRegisterNamespaceBootstrapValidatesUpdatedResponse(t *testing.T) {
	for _, name := range []string{"nil", "wrong UID", "wrong namespace", "wrong name", "missing RV", "mutated cluster", "mutated marker", "mutated digest", "missing registration", "unknown data"} {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t)
			client := fake.NewClientset(registrationConfigMap(t, f, nil))
			client.PrependReactor("update", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
				cm := action.(kt.UpdateAction).GetObject().(*api.ConfigMap).DeepCopy()
				cm.ResourceVersion = "8"
				if name == "nil" {
					return true, nil, nil
				}
				switch name {
				case "wrong UID":
					cm.UID = "replacement"
				case "wrong namespace":
					cm.Namespace = "other"
				case "wrong name":
					cm.Name = "other"
				case "missing RV":
					cm.ResourceVersion = ""
				case "mutated cluster":
					cluster := f.cluster
					cluster.Phase = Initialized
					cm.Data["cluster.json"] = mustRegistrationJSON(t, cluster)
				case "mutated marker":
					r := fixtureRegistration(f)
					r.MarkerIDs[1] = strings.Repeat("e", 32)
					cm.Data["registration.json"] = mustRegistrationJSON(t, r)
				case "mutated digest":
					r := fixtureRegistration(f)
					r.KeyDigest = strings.Repeat("f", 64)
					cm.Data["registration.json"] = mustRegistrationJSON(t, r)
				case "missing registration":
					delete(cm.Data, "registration.json")
				case "unknown data":
					cm.Data["foreign.json"] = "secret"
				}
				return true, cm, nil
			})
			if _, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs); err == nil {
				t.Fatal("accepted untrusted update response")
			}
			assertRegistrationActions(t, client, 1)
		})
	}
}

type registrationContextClient struct {
	corev1.ConfigMapInterface
	observed       context.Context
	observedUpdate context.Context
}

func (client *registrationContextClient) Update(ctx context.Context, cm *api.ConfigMap, options metav1.UpdateOptions) (*api.ConfigMap, error) {
	client.observedUpdate = ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return client.ConfigMapInterface.Update(ctx, cm, options)
}

func (client *registrationContextClient) Get(ctx context.Context, name string, options metav1.GetOptions) (*api.ConfigMap, error) {
	client.observed = ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return client.ConfigMapInterface.Get(ctx, name, options)
}

func TestNamespaceBootstrapPassesContextAndCancellation(t *testing.T) {
	f := newRegistrationFixture(t)
	fakeClient := fake.NewClientset(registrationConfigMap(t, f, nil))
	client := &registrationContextClient{ConfigMapInterface: fakeClient.CoreV1().ConfigMaps("isolated")}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := LoadNamespaceBootstrap(ctx, client, "isolated", "identity"); err != nil || client.observed != ctx {
		t.Fatal("Get context not propagated:", err)
	}
	cancel()
	if _, err := LoadNamespaceBootstrap(ctx, client, "isolated", "identity"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled load accepted:", err)
	}
	if _, err := RegisterNamespaceBootstrap(ctx, client, "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled register accepted:", err)
	}
	assertRegistrationActions(t, fakeClient, 0)
}

func TestRegisterNamespaceBootstrapPinnedUpdate(t *testing.T) {
	f := newRegistrationFixture(t)
	original := registrationConfigMap(t, f, nil)
	client := fake.NewClientset(original)
	updates := 0
	client.PrependReactor("update", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
		updates++
		cm := action.(kt.UpdateAction).GetObject().(*api.ConfigMap)
		if cm.UID != "expected-uid" || cm.ResourceVersion != "7" || cm.Namespace != "isolated" || cm.Name != "identity" {
			t.Fatal("update lost CAS identity")
		}
		if cm.Labels["release"] != "preserve" || cm.Data["cluster.json"] != original.Data["cluster.json"] {
			t.Fatal("update changed existing metadata/cluster")
		}
		if !reflect.DeepEqual(cm.ObjectMeta, original.ObjectMeta) {
			t.Fatal("update lost metadata")
		}
		if _, ok := cm.Data["registration.json"]; !ok {
			t.Fatal("missing registration update")
		}
		updated := cm.DeepCopy()
		updated.ResourceVersion = "8"
		return true, updated, nil
	})
	state, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs)
	if err != nil {
		t.Fatal("fresh registration must update:", err)
	}
	if state.Cluster.Phase != Pending || state.Registration == nil || state.ResourceVersion != "8" || updates != 1 {
		t.Fatal("invalid grant update")
	}
	if _, ok := original.Data["registration.json"]; ok {
		t.Fatal("mutated original ConfigMap")
	}
}

func TestNamespaceBootstrapRejectsInvalidCallTargets(t *testing.T) {
	f := newRegistrationFixture(t)
	for _, name := range []string{"nil client", "empty namespace", "empty name"} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewClientset(registrationConfigMap(t, f, nil))
			scoped := client.CoreV1().ConfigMaps("isolated")
			namespace, cmName := "isolated", "identity"
			switch name {
			case "nil client":
				scoped = nil
			case "empty namespace":
				namespace = ""
			case "empty name":
				cmName = ""
			}
			if _, err := LoadNamespaceBootstrap(context.Background(), scoped, namespace, cmName); err == nil {
				t.Fatal("accepted invalid target")
			}
			if _, err := RegisterNamespaceBootstrap(context.Background(), scoped, namespace, cmName, "expected-uid", "7", f.keys, f.challenges, f.proofs); err == nil {
				t.Fatal("accepted invalid registration target")
			}
			if len(client.Actions()) != 0 {
				t.Fatal("invalid target contacted API")
			}
		})
	}
}

func TestRegisterNamespaceBootstrapUnregisteredInvalidProofsNeverUpdate(t *testing.T) {
	for _, name := range []string{"configured", "signature", "Initialized"} {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t)
			switch name {
			case "configured":
				f.proofs[1] = f.sign(t, 1, f.challenges[1], configuredRegistrationObservation(f, 1, InventoryProof))
			case "signature":
				f.proofs[1].Signature = strings.Repeat("0", 128)
			case "Initialized":
				f.cluster.Phase = Initialized
			}
			client := fake.NewClientset(registrationConfigMap(t, f, nil))
			if _, err := RegisterNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs); err == nil {
				t.Fatal("invalid fresh grant accepted")
			}
			assertRegistrationActions(t, client, 0)
		})
	}
}

func TestNamespaceBootstrapCancellationAtAPIBoundaries(t *testing.T) {
	for _, boundary := range []string{"get", "update"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRegistrationFixture(t)
			cm := registrationConfigMap(t, f, nil)
			client := fake.NewClientset(cm)
			scoped := &registrationContextClient{ConfigMapInterface: client.CoreV1().ConfigMaps("isolated")}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client.PrependReactor(boundary, "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
				cancel()
				if boundary == "get" {
					return true, cm, nil
				}
				update := action.(kt.UpdateAction).GetObject().(*api.ConfigMap).DeepCopy()
				update.ResourceVersion = "8"
				return true, update, nil
			})
			if _, err := RegisterNamespaceBootstrap(ctx, scoped, "isolated", "identity", "expected-uid", "7", f.keys, f.challenges, f.proofs); !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled API result accepted:", err)
			}
			if scoped.observed != ctx {
				t.Fatal("Get context not propagated")
			}
			updates := 0
			if boundary == "update" {
				updates = 1
				if scoped.observedUpdate != ctx {
					t.Fatal("Update context not propagated")
				}
			}
			assertRegistrationActions(t, client, updates)
		})
	}
}

func TestNamespaceBootstrapRedactsGetAPIError(t *testing.T) {
	client := fake.NewClientset()
	cause := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "identity", errors.New("PRIVATE-CM-CONTENT"))
	client.PrependReactor("get", "configmaps", func(kt.Action) (bool, runtime.Object, error) { return true, nil, cause })
	_, err := LoadNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps("isolated"), "isolated", "identity")
	if !apierrors.IsForbidden(err) || !errors.Is(err, cause) || strings.Contains(err.Error(), "PRIVATE-CM-CONTENT") {
		t.Fatal("Get error redaction/type lost:", err)
	}
	assertRegistrationActions(t, client, 0)
}
