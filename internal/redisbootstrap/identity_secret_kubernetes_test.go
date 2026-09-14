package redisbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

func identityFixture(t *testing.T) (*fake.Clientset, IdentitySecretOptions) {
	t.Helper()
	o := IdentitySecretOptions{Namespace: "isolated", StatefulSetName: "redis", SecretName: "redis-identity", StateConfigMap: "state", Members: [3]string{"redis-0", "redis-1", "redis-2"}, FreshClusterID: "new-install", Timeout: time.Second}
	cluster, _ := json.Marshal(ClusterState{ClusterID: o.FreshClusterID, Members: o.Members, Phase: Pending})
	cm := &api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: o.StateConfigMap, UID: "state-uid", ResourceVersion: "1"}, Data: map[string]string{"cluster.json": string(cluster)}}
	client := fake.NewClientset(cm)
	client.PrependReactor("create", "secrets", func(a kt.Action) (bool, runtime.Object, error) {
		s := a.(kt.CreateAction).GetObject().(*api.Secret).DeepCopy()
		s.UID = "identity-uid"
		s.ResourceVersion = "1"
		err := client.Tracker().Create(api.SchemeGroupVersion.WithResource("secrets"), s, o.Namespace)
		return true, s, err
	})
	return client, o
}

func assertNoIdentityCreate(t *testing.T, c *fake.Clientset) {
	t.Helper()
	for _, a := range c.Actions() {
		if a.GetVerb() != "get" {
			t.Fatal("read-only contract violated", a.GetVerb(), a.GetResource())
		}
	}
}

func TestEnsureIdentityRejectsUnsafeFresh(t *testing.T) {
	for _, scenario := range []string{"no permit", "old cluster", "old PVC", "unmarked PVC", "PVC deleting", "missing CM old PVC", "registered missing identity", "initialized missing identity", "CM deleting", "CM UID change", "CM RV change"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			cm, _ := c.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.StateConfigMap, metav1.GetOptions{})
			switch scenario {
			case "no permit":
				o.FreshClusterID = ""
			case "old cluster":
				o.FreshClusterID = "foreign-install"
			case "registered missing identity", "initialized missing identity":
				f := newRegistrationFixture(t)
				f.cluster = ClusterState{ClusterID: o.FreshClusterID, Members: o.Members, Phase: Pending}
				r := fixtureRegistration(f)
				if scenario == "initialized missing identity" {
					f.cluster.Phase = Initialized
					r.Cluster = f.cluster
				}
				data, _ := json.Marshal(f.cluster)
				reg, _ := json.Marshal(r)
				cm.Data["cluster.json"] = string(data)
				cm.Data["registration.json"] = string(reg)
			case "CM deleting":
				now := metav1.Now()
				cm.DeletionTimestamp = &now
			case "old PVC", "unmarked PVC", "PVC deleting", "missing CM old PVC":
				pvc := &api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: "data-redis-0", UID: "pvc-uid", ResourceVersion: "1", Annotations: map[string]string{"sandbox/redis-cluster-id": o.FreshClusterID}}}
				if scenario == "old PVC" || scenario == "missing CM old PVC" {
					pvc.Annotations["sandbox/redis-cluster-id"] = "old"
				}
				if scenario == "unmarked PVC" {
					pvc.Annotations = nil
				}
				if scenario == "PVC deleting" {
					now := metav1.Now()
					pvc.DeletionTimestamp = &now
				}
				if err := c.Tracker().Create(api.SchemeGroupVersion.WithResource("persistentvolumeclaims"), pvc, o.Namespace); err != nil {
					t.Fatal(err)
				}
			case "CM UID change", "CM RV change":
				calls := 0
				c.PrependReactor("get", "configmaps", func(kt.Action) (bool, runtime.Object, error) {
					calls++
					next := cm.DeepCopy()
					if calls > 1 {
						if scenario == "CM UID change" {
							next.UID = "replacement"
						} else {
							next.ResourceVersion = "2"
						}
					}
					return true, next, nil
				})
			}
			if scenario == "missing CM old PVC" {
				if err := c.Tracker().Delete(api.SchemeGroupVersion.WithResource("configmaps"), o.Namespace, o.StateConfigMap); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), cm, o.Namespace); err != nil {
					t.Fatal(err)
				}
			}
			c.ClearActions()
			err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o)
			if err == nil {
				t.Fatal("unsafe fresh identity accepted")
			}
			if scenario == "missing CM old PVC" && !errors.Is(err, ErrIdentityMissing) {
				t.Fatal("old PVC should fail immediately, not merely time out", err)
			}
			assertNoIdentityCreate(t, c)
		})
	}
}

func installExistingIdentity(t *testing.T, c *fake.Clientset, o IdentitySecretOptions) *api.Secret {
	t.Helper()
	s, err := GenerateIdentitySecret(o.Namespace, o.StatefulSetName)
	if err != nil {
		t.Fatal(err)
	}
	s.Name = o.SecretName
	s.UID = "original"
	s.ResourceVersion = "7"
	s.Annotations = map[string]string{"custom": "preserve"}
	if err := c.Tracker().Create(api.SchemeGroupVersion.WithResource("secrets"), s, o.Namespace); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEnsureIdentityExistingValidation(t *testing.T) {
	for _, scenario := range []string{"valid external", "mutable", "wrong seed", "extra key", "invalid JSON", "wrong type", "missing UID", "missing RV", "deleting", "registration mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			o.FreshClusterID = ""
			o.SecretName = "external-identity"
			s := installExistingIdentity(t, c, o)
			switch scenario {
			case "mutable":
				b := false
				s.Immutable = &b
			case "wrong seed":
				s.Data["redis-0"][0] ^= 1
			case "extra key":
				s.Data["PRIVATE-SHOULD-NOT-LEAK"] = []byte("PRIVATE-SHOULD-NOT-LEAK")
			case "invalid JSON":
				s.Data["public-keys.json"] = []byte("PRIVATE-SHOULD-NOT-LEAK")
			case "wrong type":
				s.Type = api.SecretTypeTLS
			case "missing UID":
				s.UID = ""
			case "missing RV":
				s.ResourceVersion = ""
			case "deleting":
				now := metav1.Now()
				s.DeletionTimestamp = &now
			case "registration mismatch":
				f := newRegistrationFixture(t)
				f.cluster = ClusterState{ClusterID: "new-install", Members: o.Members, Phase: Pending}
				r := fixtureRegistration(f)
				reg, _ := json.Marshal(r)
				cm, _ := c.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.StateConfigMap, metav1.GetOptions{})
				cm.Data["registration.json"] = string(reg)
				_ = c.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), cm, o.Namespace)
			}
			_ = c.Tracker().Update(api.SchemeGroupVersion.WithResource("secrets"), s, o.Namespace)
			c.ClearActions()
			err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o)
			if (err == nil) != (scenario == "valid external") {
				t.Fatal("wrong validation outcome", err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("private content leaked")
			}
			after, _ := c.CoreV1().Secrets(o.Namespace).Get(context.Background(), o.SecretName, metav1.GetOptions{})
			if !reflect.DeepEqual(s, after) {
				t.Fatal("existing identity changed")
			}
			assertNoIdentityCreate(t, c)
		})
	}
}

func TestEnsureIdentityConcurrentAndUncertainCreate(t *testing.T) {
	for _, scenario := range []string{"already exists", "committed timeout", "uncommitted timeout"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			o.Timeout = 300 * time.Millisecond
			var server *api.Secret
			c.PrependReactor("create", "secrets", func(a kt.Action) (bool, runtime.Object, error) {
				if scenario != "uncommitted timeout" {
					server = installExistingIdentity(t, c, o)
				}
				if scenario == "already exists" {
					return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, o.SecretName)
				}
				return true, nil, errors.New("PRIVATE-API-REJECTED-BODY")
			})
			err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o)
			if (err == nil) != (scenario != "uncommitted timeout") {
				t.Fatal("uncertain create incorrectly resolved", err)
			}
			if err != nil && strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("API body leaked")
			}
			creates := 0
			for _, a := range c.Actions() {
				if a.GetVerb() == "create" {
					creates++
				}
				if a.GetVerb() != "get" && a.GetVerb() != "create" {
					t.Fatal("forbidden mutation")
				}
			}
			if creates != 1 {
				t.Fatal("retried generation/create", creates)
			}
			if server != nil {
				got, _ := c.CoreV1().Secrets(o.Namespace).Get(context.Background(), o.SecretName, metav1.GetOptions{})
				if !reflect.DeepEqual(server, got) {
					t.Fatal("concurrent original overwritten")
				}
			}
		})
	}
}

func TestEnsureIdentityCancellation(t *testing.T) {
	c, o := identityFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := EnsureIdentitySecret(ctx, c.CoreV1(), o); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(c.Actions()) != 0 {
		t.Fatal("API accessed after cancellation")
	}
}

func TestEnsureIdentityPostCreatePVCPin(t *testing.T) {
	for _, scenario := range []string{"replaced", "disappeared"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			pvc := &api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: "data-redis-0", UID: "original-pvc", ResourceVersion: "1", Annotations: map[string]string{IdentityClusterAnnotation: o.FreshClusterID}}}
			_ = c.Tracker().Create(api.SchemeGroupVersion.WithResource("persistentvolumeclaims"), pvc, o.Namespace)
			c.PrependReactor("create", "secrets", func(a kt.Action) (bool, runtime.Object, error) {
				_ = c.Tracker().Delete(api.SchemeGroupVersion.WithResource("persistentvolumeclaims"), o.Namespace, pvc.Name)
				if scenario == "replaced" {
					replacement := pvc.DeepCopy()
					replacement.UID = "replacement-pvc"
					_ = c.Tracker().Create(api.SchemeGroupVersion.WithResource("persistentvolumeclaims"), replacement, o.Namespace)
				}
				return false, nil, nil
			})
			if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); err == nil {
				t.Fatal("post-create PVC identity drift accepted")
			}
		})
	}
}

func TestEnsureIdentityWaitsForCMWithNewPVC(t *testing.T) {
	c, o := identityFixture(t)
	pvc := &api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: "data-redis-0", UID: "new-pvc", ResourceVersion: "1", Annotations: map[string]string{IdentityClusterAnnotation: o.FreshClusterID}}}
	_ = c.Tracker().Create(api.SchemeGroupVersion.WithResource("persistentvolumeclaims"), pvc, o.Namespace)
	calls := 0
	c.PrependReactor("get", "configmaps", func(kt.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, o.StateConfigMap)
		}
		return false, nil, nil
	})
	if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); err != nil {
		t.Fatal("new ordinary resources can be observed out of order", err)
	}
}

func TestEnsureIdentityRetriesTransientReads(t *testing.T) {
	for _, scenario := range []string{"CM second", "PVC before", "secret final", "PVC after"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			o.Timeout = 2 * time.Second
			resource := "configmaps"
			failAt := 2
			switch scenario {
			case "PVC before":
				resource = "persistentvolumeclaims"
				failAt = 1
			case "secret final":
				resource = "secrets"
				failAt = 2
			case "PVC after":
				resource = "persistentvolumeclaims"
				failAt = 7
			}
			calls := 0
			c.PrependReactor("get", resource, func(kt.Action) (bool, runtime.Object, error) {
				calls++
				if calls == failAt {
					return true, nil, apierrors.NewServiceUnavailable("PRIVATE-TRANSIENT-API-BODY")
				}
				return false, nil, nil
			})
			if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); err != nil {
				t.Fatal("transient read did not recover", err)
			}
			creates := 0
			for _, a := range c.Actions() {
				if a.GetVerb() == "create" {
					creates++
				}
			}
			if creates != 1 {
				t.Fatal("create retried", creates)
			}
		})
	}
}

func TestEnsureIdentityRegisteredReuse(t *testing.T) {
	for _, phase := range []Phase{Pending, Initialized} {
		t.Run(string(phase), func(t *testing.T) {
			c, o := identityFixture(t)
			s := installExistingIdentity(t, c, o)
			keys, err := ParseMemberPublicKeys(s.Data["public-keys.json"])
			if err != nil {
				t.Fatal(err)
			}
			digest, _ := PublicKeySetDigest(keys)
			f := newRegistrationFixture(t)
			f.cluster = ClusterState{ClusterID: o.FreshClusterID, Members: o.Members, Phase: phase}
			r := fixtureRegistration(f)
			r.KeyDigest = digest
			cm, _ := c.CoreV1().ConfigMaps(o.Namespace).Get(context.Background(), o.StateConfigMap, metav1.GetOptions{})
			cluster, _ := json.Marshal(f.cluster)
			reg, _ := json.Marshal(r)
			cm.Data["cluster.json"] = string(cluster)
			cm.Data["registration.json"] = string(reg)
			_ = c.Tracker().Update(api.SchemeGroupVersion.WithResource("configmaps"), cm, o.Namespace)
			c.ClearActions()
			o.FreshClusterID = ""
			if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); err != nil {
				t.Fatal("valid retained registration rejected", err)
			}
			assertNoIdentityCreate(t, c)
		})
	}
}

func TestEnsureIdentityMissingCMBudgetAndNoPermit(t *testing.T) {
	for _, scenario := range []string{"never created", "matching PVC no permit"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			o.Timeout = 30 * time.Millisecond
			_ = c.Tracker().Delete(api.SchemeGroupVersion.WithResource("configmaps"), o.Namespace, o.StateConfigMap)
			if scenario == "matching PVC no permit" {
				pvc := &api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: "data-redis-0", UID: "new-pvc", ResourceVersion: "1", Annotations: map[string]string{IdentityClusterAnnotation: o.FreshClusterID}}}
				_ = c.Tracker().Create(api.SchemeGroupVersion.WithResource("persistentvolumeclaims"), pvc, o.Namespace)
				o.FreshClusterID = ""
			}
			err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o)
			if scenario == "never created" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("missing ordinary CM wait was not bounded", err)
			}
			if scenario == "matching PVC no permit" && !errors.Is(err, ErrIdentityMissing) {
				t.Fatal("PVC marker granted creation without fresh permit", err)
			}
			assertNoIdentityCreate(t, c)
		})
	}
}

func TestEnsureIdentityInvalidScopeNeverAccessesAPI(t *testing.T) {
	for _, scenario := range []string{"namespace", "statefulset", "long statefulset", "secret IP", "CM IP", "duplicate member", "IP member", "external create", "invalid CID", "negative timeout", "long timeout"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			switch scenario {
			case "namespace":
				o.Namespace = "invalid_namespace"
			case "statefulset":
				o.StatefulSetName = "UpperCase"
			case "long statefulset":
				o.StatefulSetName = strings.Repeat("a", 55)
			case "secret IP":
				o.SecretName = "127.0.0.1"
			case "CM IP":
				o.StateConfigMap = "127.0.0.1"
			case "duplicate member":
				o.Members[1] = o.Members[0]
			case "IP member":
				o.Members[0] = "127.0.0.1"
			case "external create":
				o.SecretName = "external-identity"
			case "invalid CID":
				o.FreshClusterID = "PRIVATE\nINVALID"
			case "negative timeout":
				o.Timeout = -time.Second
			case "long timeout":
				o.Timeout = 3 * time.Minute
			}
			if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); !errors.Is(err, ErrIdentityInvalid) {
				t.Fatal("invalid scope not rejected", err)
			}
			if len(c.Actions()) != 0 {
				t.Fatal("API accessed for invalid scope")
			}
		})
	}
	c, o := identityFixture(t)
	if err := EnsureIdentitySecret(nil, c.CoreV1(), o); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatal(err)
	}
	if err := EnsureIdentitySecret(context.Background(), nil, o); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatal(err)
	}
}

func TestEnsureIdentityCreatesAndPreserves(t *testing.T) {
	client, o := identityFixture(t)
	if err := EnsureIdentitySecret(context.Background(), client.CoreV1(), o); err != nil {
		t.Fatal(err)
	}
	before, err := client.CoreV1().Secrets(o.Namespace).Get(context.Background(), o.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal("automatic identity was not created", err)
	}
	if len(before.Data) != 4 || before.Immutable == nil || !*before.Immutable || before.Annotations["helm.sh/resource-policy"] != "keep" || len(before.OwnerReferences) != 0 {
		t.Fatal("unsafe created identity contract")
	}
	keys, err := ParseMemberPublicKeys(before.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := ParseMemberPrivateSeed(before.Data[o.StatefulSetName+"-"+string(rune('0'+i))], keys, i); err != nil {
			t.Fatal(err)
		}
	}
	o.FreshClusterID = ""
	if err := EnsureIdentitySecret(context.Background(), client.CoreV1(), o); err != nil {
		t.Fatal(err)
	}
	after, _ := client.CoreV1().Secrets(o.Namespace).Get(context.Background(), o.SecretName, metav1.GetOptions{})
	if !reflect.DeepEqual(before, after) {
		t.Fatal("existing identity mutated")
	}
	creates := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			creates++
		}
		if a.GetVerb() != "get" && a.GetVerb() != "create" {
			t.Fatal("forbidden API verb", a.GetVerb())
		}
	}
	if creates != 1 {
		t.Fatal("identity recreated", creates)
	}
}
