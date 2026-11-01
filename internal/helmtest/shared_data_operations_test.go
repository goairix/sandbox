//go:build helmtests

package helmtest

import (
	"reflect"
	"testing"
)

func TestSharedDataHelmGateDoesNotChangeBackendIdentity(t *testing.T) {
	base := renderEtcdDefaults(t)
	changed := renderEtcdDefaults(t, "config.storage.state.sharedDataOperations=true")
	for _, pair := range [][2]string{{"StatefulSet", "sandbox-etcd"}, {"ConfigMap", "sandbox-backend-fingerprint"}} {
		if !reflect.DeepEqual(object(t, base, pair[0], pair[1]), object(t, changed, pair[0], pair[1])) {
			t.Fatalf("coordination toggle mutated %s", pair[1])
		}
	}
	for _, sample := range []struct {
		docs []map[string]any
		want string
	}{{base, "false"}, {changed, "true"}} {
		deployment := object(t, sample.docs, "Deployment", "sandbox-api")
		pod := mapping(t, mapping(t, mapping(t, deployment["spec"])["template"])["spec"])
		vars := env(t, container(t, pod, "containers", "sandbox"))
		item, ok := vars["SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS"]
		if !ok || item["value"] != sample.want {
			t.Fatalf("missing or incorrect operation gate, expected %s", sample.want)
		}
	}
}
