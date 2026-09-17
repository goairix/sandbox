package redisbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	api "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kt "k8s.io/client-go/testing"
)

func TestIdentityPVCBindingVersionAdvance(t *testing.T) {
	c, o := identityFixture(t)
	reads := 0
	c.PrependReactor("get", "persistentvolumeclaims", func(a kt.Action) (bool, runtime.Object, error) {
		if a.(kt.GetAction).GetName() != "data-redis-0" {
			return false, nil, nil
		}
		reads++
		pvc := &api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "data-redis-0", Namespace: o.Namespace, UID: "pvc-original", ResourceVersion: "1",
			Annotations: map[string]string{IdentityClusterAnnotation: o.FreshClusterID},
		}}
		if reads > 1 {
			pvc.ResourceVersion = "2"
		}
		return true, pvc, nil
	})
	if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); err != nil {
		t.Fatal("ordinary PVC binding must not invalidate a fresh installation", err)
	}
	creates := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "secrets" {
			creates++
		}
	}
	if creates != 1 || reads != 5 {
		t.Fatalf("expected one create after two stable reads and post-check; creates=%d PVC reads=%d", creates, reads)
	}
}

func bindingPVC(o IdentitySecretOptions, version string) *api.PersistentVolumeClaim {
	return &api.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-redis-0", Namespace: o.Namespace, UID: "pvc-original", ResourceVersion: version,
		Annotations: map[string]string{IdentityClusterAnnotation: o.FreshClusterID},
	}}
}

func TestIdentityPVCBindingMultipleAdvancesAndStableRequestBudget(t *testing.T) {
	for _, driftingRounds := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(driftingRounds), func(t *testing.T) {
			c, o := identityFixture(t)
			o.Timeout = 2 * time.Second
			reads := 0
			c.PrependReactor("get", "persistentvolumeclaims", func(a kt.Action) (bool, runtime.Object, error) {
				if a.(kt.GetAction).GetName() != "data-redis-0" {
					return false, nil, nil
				}
				reads++
				version := min(reads, driftingRounds*2+1)
				return true, bindingPVC(o, fmt.Sprint(version)), nil
			})
			if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, a := range c.Actions() {
				counts[a.GetVerb()+"/"+a.GetResource().Resource]++
			}
			want := map[string]int{"get/configmaps": 3 + driftingRounds, "get/persistentvolumeclaims": 9 + 6*driftingRounds, "get/secrets": 3, "create/secrets": 1}
			if !reflect.DeepEqual(want, counts) {
				t.Fatalf("request budget changed: got=%v want=%v", counts, want)
			}
		})
	}
}

func TestIdentityPVCBindingRetryCannotResetPinnedIdentity(t *testing.T) {
	for _, scenario := range []string{"replacement", "disappeared", "deleting", "annotation", "missing UID", "missing RV", "name", "namespace", "new PVC replaced", "new PVC disappeared"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			reads := 0
			c.PrependReactor("get", "persistentvolumeclaims", func(a kt.Action) (bool, runtime.Object, error) {
				if a.(kt.GetAction).GetName() != "data-redis-0" {
					return false, nil, nil
				}
				reads++
				// Exercise first appearance during a drifting round as well as
				// a PVC which existed before the initial observations.
				pvc := bindingPVC(o, fmt.Sprint(min(reads, 2)))
				if scenario == "new PVC replaced" || scenario == "new PVC disappeared" {
					if reads == 1 {
						return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, pvc.Name)
					}
					// Another previously present PVC forces stabilization retry.
				}
				if reads >= 3 {
					switch scenario {
					case "replacement", "new PVC replaced":
						pvc.UID = "replacement"
					case "disappeared", "new PVC disappeared":
						return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, pvc.Name)
					case "deleting":
						now := metav1.Now()
						pvc.DeletionTimestamp = &now
					case "annotation":
						pvc.Annotations[IdentityClusterAnnotation] = "foreign"
					case "missing UID":
						pvc.UID = ""
					case "missing RV":
						pvc.ResourceVersion = ""
					case "name":
						pvc.Name = "unexpected"
					case "namespace":
						pvc.Namespace = "unexpected"
					}
				}
				return true, pvc, nil
			})
			otherReads := 0
			c.PrependReactor("get", "persistentvolumeclaims", func(a kt.Action) (bool, runtime.Object, error) {
				if a.(kt.GetAction).GetName() != "data-redis-1" {
					return false, nil, nil
				}
				otherReads++
				pvc := bindingPVC(o, fmt.Sprint(min(otherReads, 2)))
				pvc.Name, pvc.UID = "data-redis-1", "pvc-other"
				return true, pvc, nil
			})
			if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); !errors.Is(err, ErrIdentityInvalid) {
				t.Fatal("retry accepted a changed installation", err)
			}
			assertNoIdentityCreate(t, c)
		})
	}
}

func TestIdentityPVCBindingSharedDeadlineAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			c, o := identityFixture(t)
			o.Timeout = 300 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			c.PrependReactor("get", "persistentvolumeclaims", func(a kt.Action) (bool, runtime.Object, error) {
				if a.(kt.GetAction).GetName() != "data-redis-0" {
					return false, nil, nil
				}
				reads++
				if cancelled && reads == 2 {
					cancel()
				}
				return true, bindingPVC(o, fmt.Sprint(reads)), nil
			})
			want := context.DeadlineExceeded
			if cancelled {
				want = context.Canceled
			}
			if err := EnsureIdentitySecret(ctx, c.CoreV1(), o); !errors.Is(err, want) {
				t.Fatal("shared budget was not respected", err)
			}
			assertNoIdentityCreate(t, c)
		})
	}
}

func TestIdentityPVCBindingRejectsStateChangeBetweenRetries(t *testing.T) {
	for _, scenario := range []string{"UID", "RV", "cluster", "members", "phase", "registration", "deleting"} {
		t.Run(scenario, func(t *testing.T) {
			c, o := identityFixture(t)
			pvcReads := 0
			c.PrependReactor("get", "persistentvolumeclaims", func(a kt.Action) (bool, runtime.Object, error) {
				if a.(kt.GetAction).GetName() != "data-redis-0" {
					return false, nil, nil
				}
				pvcReads++
				return true, bindingPVC(o, fmt.Sprint(min(pvcReads, 2))), nil
			})
			cmReads := 0
			c.PrependReactor("get", "configmaps", func(kt.Action) (bool, runtime.Object, error) {
				cmReads++
				obj, err := c.Tracker().Get(api.SchemeGroupVersion.WithResource("configmaps"), o.Namespace, o.StateConfigMap)
				if err != nil {
					return true, nil, err
				}
				cm := obj.(*api.ConfigMap).DeepCopy()
				if cmReads >= 3 {
					cluster := ClusterState{ClusterID: o.FreshClusterID, Members: o.Members, Phase: Pending}
					switch scenario {
					case "UID":
						cm.UID = "replacement"
					case "RV":
						cm.ResourceVersion = "2"
					case "cluster":
						cluster.ClusterID = "foreign"
					case "members":
						cluster.Members[2] = "foreign"
					case "phase":
						cluster.Phase = Initialized
					case "registration":
						f := newRegistrationFixture(t)
						f.cluster = cluster
						data, err := json.Marshal(fixtureRegistration(f))
						if err != nil {
							t.Fatal(err)
						}
						cm.Data["registration.json"] = string(data)
					case "deleting":
						now := metav1.Now()
						cm.DeletionTimestamp = &now
					}
					data, err := json.Marshal(cluster)
					if err != nil {
						t.Fatal(err)
					}
					cm.Data["cluster.json"] = string(data)
				}
				return true, cm, nil
			})
			if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); !errors.Is(err, ErrIdentityInvalid) {
				t.Fatal("changed state accepted after PVC retry", err)
			}
			assertNoIdentityCreate(t, c)
		})
	}
}

func TestIdentityPVCBindingConcurrentSecretAfterStabilization(t *testing.T) {
	c, o := identityFixture(t)
	var original *api.Secret
	reads := 0
	c.PrependReactor("get", "persistentvolumeclaims", func(a kt.Action) (bool, runtime.Object, error) {
		if a.(kt.GetAction).GetName() != "data-redis-0" {
			return false, nil, nil
		}
		reads++
		if reads == 4 {
			original = installExistingIdentity(t, c, o).DeepCopy()
		}
		return true, bindingPVC(o, fmt.Sprint(min(reads, 2))), nil
	})
	if err := EnsureIdentitySecret(context.Background(), c.CoreV1(), o); err != nil {
		t.Fatal(err)
	}
	assertNoIdentityCreate(t, c)
	stored, err := c.CoreV1().Secrets(o.Namespace).Get(context.Background(), o.SecretName, metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(original, stored) {
		t.Fatal("concurrent identity was not preserved", err)
	}
}
