//go:build helmtests

package helmtest

import "testing"

func TestSharedDataHelmNeedsNoDeploymentSwitch(t *testing.T) {
	docs := renderEtcdDefaults(t)
	deployment := object(t, docs, "Deployment", "sandbox-api")
	pod := mapping(t, mapping(t, mapping(t, deployment["spec"])["template"])["spec"])
	vars := env(t, container(t, pod, "containers", "sandbox"))
	if _, ok := vars["SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS"]; ok {
		t.Fatal("shared coordination must be automatic without a deployment switch")
	}
}
