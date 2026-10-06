package etcd

import (
	"errors"
	"strings"
	"testing"
)

func TestDomainKeysPaths(t *testing.T) {
	n, err := NewNamespace("/sandbox", "scope", "cell")
	if err != nil {
		t.Fatal(err)
	}
	w, _ := NewWorkspaceIdentity("minio", "store", "bucket", "work/")
	owner, fence, err := n.workspaceKeys(w)
	root := "/sandbox/scope/cell/"
	if err != nil || owner != root+"p/1b/workspaces/1bab2aa6b5393553247aaca84415eed0c40a14e1436b8b152ea171249cd920dc/owner" || fence != root+"p/1b/workspaces/1bab2aa6b5393553247aaca84415eed0c40a14e1436b8b152ea171249cd920dc/fence" {
		t.Fatalf("workspace keys %q %q: %v", owner, fence, err)
	}
	control, snapshot, err := n.sandboxKeys(0x0a, "sandbox-1", "v1")
	if err != nil || control != root+"p/0a/controls/sandbox-1" || snapshot != root+"p/0a/snapshots/sandbox-1/v1" {
		t.Fatalf("sandbox keys %q %q: %v", control, snapshot, err)
	}
	intent, err := n.intentKey(255, "intent-1")
	if err != nil || intent != root+"p/ff/intents/intent-1" {
		t.Fatalf("intent %q %v", intent, err)
	}
	request, err := n.requestKey(strings.Repeat("a", 64))
	if err != nil || request != root+"requests/"+strings.Repeat("a", 64) {
		t.Fatalf("request %q %v", request, err)
	}
}
func TestDomainKeysRejectEscapesAndZeroIdentity(t *testing.T) {
	n, _ := NewNamespace("/sandbox", "scope", "cell")
	if _, _, err := n.workspaceKeys(WorkspaceIdentity{}); !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("zero workspace: %v", err)
	}
	for _, id := range []string{"", ".", "..", "../escape", "a/b", strings.Repeat("a", 129), "中文"} {
		if _, _, err := n.sandboxKeys(0, id, "v1"); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("sandbox %q: %v", id, err)
		}
		if _, _, err := n.sandboxKeys(0, "sandbox", id); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("version %q: %v", id, err)
		}
		if _, err := n.intentKey(0, id); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("intent %q: %v", id, err)
		}
	}
	for _, hash := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63), "../escape"} {
		if _, err := n.requestKey(hash); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("request hash: %v", err)
		}
	}
	if _, err := (Namespace{}).intentKey(0, "intent"); err == nil {
		t.Error("zero namespace accepted")
	}
}
func TestDomainPlacementKey(t *testing.T) {
	n, _ := NewNamespace("/sandbox", "scope", "cell")
	key, err := n.placementKey("sandbox-1")
	if err != nil || key != "/sandbox/scope/cell/indexes/sandbox/sandbox-1" {
		t.Fatalf("placement %q %v", key, err)
	}
	for _, id := range []string{"", "../escape", strings.Repeat("a", 129)} {
		if _, err := n.placementKey(id); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("invalid placement id accepted: %v", err)
		}
	}
}
