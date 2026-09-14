package redisbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func initializeFixture(t *testing.T, phase Phase) (*topologyFixture, *fake.Clientset, InitializeOptions) {
	t.Helper()
	f := newTopologyFixture(t)
	f.cluster.Phase = phase
	r := f.options.Registration
	r.Cluster.Phase = phase
	f.options.Registration = r
	client := fake.NewClientset(registrationConfigMap(t, f.registrationFixture, &r))
	// Unlike the simple tracker, emulate server-side resourceVersion CAS and
	// increment the version. This still isn't a real Kubernetes API integration.
	client.PrependReactor("update", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
		u := action.(kt.UpdateAction).GetObject().(*api.ConfigMap)
		obj, err := client.Tracker().Get(api.SchemeGroupVersion.WithResource("configmaps"), u.Namespace, u.Name)
		if err != nil {
			return true, nil, err
		}
		current := obj.(*api.ConfigMap)
		if u.UID != current.UID || u.ResourceVersion != current.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, u.Name, errors.New("PRIVATE-API-PAYLOAD"))
		}
		updated := u.DeepCopy()
		rv, err := strconv.Atoi(current.ResourceVersion)
		if err != nil {
			t.Fatal(err)
		}
		updated.ResourceVersion = strconv.Itoa(rv + 1)
		if err := client.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), updated, u.Namespace); err != nil {
			return true, nil, err
		}
		return true, updated, nil
	})
	o := InitializeOptions{Namespace: "isolated", Name: "identity", PublicKeys: f.keys, MasterName: f.options.MasterName, DataPassword: f.options.DataPassword, SentinelPassword: f.options.SentinelPassword, AckTimeout: time.Second, Timeout: 2 * time.Second}
	return f, client, o
}

func TestInitializeActualTopologyACKPinnedPhaseCAS(t *testing.T) {
	f, client, o := initializeFixture(t, Pending)
	original, err := client.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies}); err != nil {
		t.Fatal("valid live topology+ACK must open namespace gate:", err)
	}
	assertRegistrationActions(t, client, 1)
	cm, err := client.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseNamespaceBootstrap(cm, o.Namespace, o.Name)
	if err != nil || s.Cluster.Phase != Initialized || s.Registration == nil || s.Registration.Cluster != s.Cluster || s.UID != original.UID || s.ResourceVersion != "8" {
		t.Fatal("atomic matching cluster/registration phase and identity CAS missing")
	}
	if s.Registration.MarkerIDs != f.options.Registration.MarkerIDs || s.Registration.KeyDigest != f.options.Registration.KeyDigest {
		t.Fatal("initialization changed retained grant")
	}
	before, after := original.DeepCopy(), cm.DeepCopy()
	before.Data = nil
	after.Data = nil
	before.ResourceVersion = ""
	after.ResourceVersion = ""
	if !reflect.DeepEqual(before, after) {
		t.Fatal("phase update changed unrelated metadata")
	}
	if f.barriers.Load() != 1 || f.waits.Load() != 1 {
		t.Fatal("phase must follow actual new barrier ACK")
	}
}

func TestInitializeAlreadyInitializedVerifiesWithoutMutation(t *testing.T) {
	f, client, o := initializeFixture(t, Initialized)
	if err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies}); err != nil {
		t.Fatal("Initialized live topology must be checked, not reset:", err)
	}
	assertRegistrationActions(t, client, 0)
	if f.barriers.Load() != 1 {
		t.Fatal("existing phase cannot replace current ACK verification")
	}
}

func TestInitializeACKFailureNeverCommitsPhase(t *testing.T) {
	f, client, o := initializeFixture(t, Pending)
	f.ack = 0
	o.Timeout = time.Second
	err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies})
	if err == nil || strings.Contains(err.Error(), o.DataPassword) {
		t.Fatal("unconfirmed ACK was swallowed/leaked")
	}
	assertRegistrationActions(t, client, 0)
	cm, err := client.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseNamespaceBootstrap(cm, o.Namespace, o.Name)
	if err != nil || s.Cluster.Phase != Pending {
		t.Fatal("failed ACK opened gate")
	}
	if f.barriers.Load() < 1 || f.waits.Load() < 1 {
		t.Fatal("must actually attempt the new write and WAIT")
	}
}

func TestInitializeFreshReservedRegistrationThenLiveTopology(t *testing.T) {
	f, client, o := initializeFixture(t, Pending)
	cm, err := client.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delete(cm.Data, "registration.json")
	if err := client.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), cm, o.Namespace); err != nil {
		t.Fatal(err)
	}
	configured := f.observations
	for i := range 3 {
		f.observations[i] = f.proofs[i].Observation
	}
	// Publish Configured observations only after actual registration update.
	client.PrependReactor("update", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
		u := action.(kt.UpdateAction).GetObject().(*api.ConfigMap)
		cluster, err := ParseClusterState([]byte(u.Data["cluster.json"]))
		if err != nil {
			t.Fatal(err)
		}
		if cluster.Phase == Pending && u.Data["registration.json"] != "" {
			f.observations = configured
		}
		return false, nil, nil
	})
	client.ClearActions()
	if err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies}); err != nil {
		t.Fatal("3 real reserved proofs must register before configured live ACK:", err)
	}
	assertRegistrationActions(t, client, 2)
	if f.proofRequests.Load() != 9 || f.barriers.Load() != 1 {
		t.Fatal("fresh grant must have 3 reserved proofs then 3 configured inventories and 3 current live proofs")
	}
	obj, err := client.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var registration BootstrapRegistration
	if err := json.Unmarshal([]byte(obj.Data["registration.json"]), &registration); err != nil || registration.MarkerIDs != f.options.Registration.MarkerIDs {
		t.Fatal("registration changed original reserved identities")
	}
}

func TestInitializePhaseConflictReverifiesWholeActualTopology(t *testing.T) {
	f, client, o := initializeFixture(t, Pending)
	conflicted := false
	client.PrependReactor("update", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		u := action.(kt.UpdateAction).GetObject().(*api.ConfigMap)
		obj, err := client.Tracker().Get(api.SchemeGroupVersion.WithResource("configmaps"), u.Namespace, u.Name)
		if err != nil {
			t.Fatal(err)
		}
		cm := obj.(*api.ConfigMap).DeepCopy()
		cm.ResourceVersion = "8"
		cm.Labels["concurrent"] = "preserve"
		if err := client.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), cm, u.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, u.Name, errors.New("PRIVATE-API-PAYLOAD"))
	})
	if err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies}); err != nil {
		t.Fatal("same identity RV conflict must retry a fresh full topology:", err)
	}
	assertRegistrationActions(t, client, 2)
	if f.barriers.Load() != 2 || f.waits.Load() != 2 || f.proofRequests.Load() != 12 {
		t.Fatal("CAS conflict borrowed a new RV without repeating actual inventory/live/write/ACK")
	}
	cm, err := client.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cm.Labels["concurrent"] != "preserve" || cm.ResourceVersion != "9" {
		t.Fatal("retry did not preserve concurrent metadata/version")
	}
}

func TestInitializeUIDReplacementAfterACKNeverMutatesReplacement(t *testing.T) {
	f, client, o := initializeFixture(t, Pending)
	client.PrependReactor("get", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
		if f.waits.Load() == 0 {
			return false, nil, nil
		}
		obj, err := client.Tracker().Get(api.SchemeGroupVersion.WithResource("configmaps"), o.Namespace, o.Name)
		if err != nil {
			t.Fatal(err)
		}
		cm := obj.(*api.ConfigMap).DeepCopy()
		cm.UID = "replacement-uid"
		return true, cm, nil
	})
	err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies})
	if err == nil || err.Error() != errInitialize.Error() {
		t.Fatal("same-name replacement after ACK must fail with redacted error")
	}
	assertRegistrationActions(t, client, 0)
	if f.barriers.Load() != 1 {
		t.Fatal("test did not reach post-ACK pinned re-read")
	}
}

func TestInitializeChangedGrantAfterACKNeverMutates(t *testing.T) {
	for _, name := range []string{"marker", "digest", "registration removed", "cluster id", "members", "unknown payload"} {
		t.Run(name, func(t *testing.T) {
			f, client, o := initializeFixture(t, Pending)
			client.PrependReactor("get", "configmaps", func(action kt.Action) (bool, runtime.Object, error) {
				if f.waits.Load() == 0 {
					return false, nil, nil
				}
				obj, err := client.Tracker().Get(api.SchemeGroupVersion.WithResource("configmaps"), o.Namespace, o.Name)
				if err != nil {
					t.Fatal(err)
				}
				cm := obj.(*api.ConfigMap).DeepCopy()
				r := f.options.Registration
				switch name {
				case "marker":
					r.MarkerIDs[2] = strings.Repeat("f", 32)
				case "digest":
					r.KeyDigest = strings.Repeat("f", 64)
				case "registration removed":
					delete(cm.Data, "registration.json")
				case "cluster id":
					r.Cluster.ClusterID = "foreign-cluster"
					cm.Data["cluster.json"] = mustRegistrationJSON(t, r.Cluster)
				case "members":
					r.Cluster.Members[2] = "foreign-member"
					cm.Data["cluster.json"] = mustRegistrationJSON(t, r.Cluster)
				case "unknown payload":
					cm.Data["PRIVATE-API-PAYLOAD"] = "secret"
				}
				if name != "registration removed" {
					cm.Data["registration.json"] = mustRegistrationJSON(t, r)
				}
				return true, cm, nil
			})
			if err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies}); err == nil || strings.Contains(err.Error(), "PRIVATE-API-PAYLOAD") {
				t.Fatal("changed grant/payload accepted or leaked")
			}
			assertRegistrationActions(t, client, 0)
			if f.barriers.Load() != 1 {
				t.Fatal("grant change must stop after its first verified topology")
			}
		})
	}
}

func TestInitializeInvalidInputAndCallerCancellationDoNotContact(t *testing.T) {
	for _, name := range []string{"nil context", "cancelled", "namespace", "name", "password", "same passwords", "keys", "ack", "timeout"} {
		t.Run(name, func(t *testing.T) {
			f, client, o := initializeFixture(t, Pending)
			ctx := context.Background()
			switch name {
			case "nil context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "namespace":
				o.Namespace = "other.namespace"
			case "name":
				o.Name = "PRIVATE-API-PAYLOAD\n"
			case "password":
				o.DataPassword = "PRIVATE-API-PAYLOAD"
			case "same passwords":
				o.SentinelPassword = o.DataPassword
			case "keys":
				o.PublicKeys[2] = o.PublicKeys[0]
			case "ack":
				o.AckTimeout = time.Millisecond
			case "timeout":
				o.Timeout = 13 * time.Minute
			}
			err := initializeNamespaceBootstrap(ctx, client.CoreV1().ConfigMaps("isolated"), o, initializeDependencies{topology: f.dependencies})
			if err == nil || len(client.Actions()) != 0 || f.proofRequests.Load() != 0 {
				t.Fatal("invalid/cancelled input contacted API/members")
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation lost")
			}
		})
	}
}

func TestInitializeFreshInvalidVolumesCannotAcquireGrant(t *testing.T) {
	for _, name := range []string{"configured before grant", "empty member", "replayed reserved proof"} {
		t.Run(name, func(t *testing.T) {
			f, client, o := initializeFixture(t, Pending)
			o.Timeout = 300 * time.Millisecond
			cm, err := client.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			delete(cm.Data, "registration.json")
			if err := client.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), cm, o.Namespace); err != nil {
				t.Fatal(err)
			}
			if name != "configured before grant" {
				for i := range 3 {
					f.observations[i] = f.proofs[i].Observation
				}
			}
			if name == "empty member" {
				f.observations[2] = LocalObservation{Volume: VolumeState{Empty: true}}
			}
			if name == "replayed reserved proof" {
				data, err := json.Marshal(f.proofs[2])
				if err != nil {
					t.Fatal(err)
				}
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(data)
				}))
				defer s.Close()
				f.urls[2] = s.URL + "/v1/identity"
			}
			client.ClearActions()
			if err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies}); err == nil {
				t.Fatal("invalid/unfresh PVC proofs acquired a grant")
			}
			assertRegistrationActions(t, client, 0)
			if f.barriers.Load() != 0 {
				t.Fatal("unregistered unsafe identity reached business write verification")
			}
		})
	}
}

func TestInitializeTransientMissingConfigMapRetriesButNeverCreates(t *testing.T) {
	f, client, o := initializeFixture(t, Pending)
	gets := 0
	client.PrependReactor("get", "configmaps", func(kt.Action) (bool, runtime.Object, error) {
		gets++
		if gets <= 2 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, o.Name)
		}
		return false, nil, nil
	})
	if err := initializeNamespaceBootstrap(context.Background(), client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies}); err != nil {
		t.Fatal("ordinary Job creation order race must wait for named existing object:", err)
	}
	assertRegistrationActions(t, client, 1)
}

func TestInitializeCallerDeadlineWhileConfigMapMissingIsPreserved(t *testing.T) {
	f, _, o := initializeFixture(t, Pending)
	client := fake.NewClientset()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := initializeNamespaceBootstrap(ctx, client.CoreV1().ConfigMaps(o.Namespace), o, initializeDependencies{topology: f.dependencies})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("caller wait deadline must not be hidden")
	}
	assertRegistrationActions(t, client, 0)
	if f.proofRequests.Load() != 0 {
		t.Fatal("missing installation identity must not contact members")
	}
}
