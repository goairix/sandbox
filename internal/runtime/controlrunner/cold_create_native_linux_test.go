//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestColdPreparationNativeFixture(t *testing.T) {
	coldCreateNativeFixture(t, "^TestColdPreparationReservation.*Native$")
}

func TestSnapshotReadNativeFixture(t *testing.T) {
	coldCreateNativeFixture(t, "^TestSnapshotRead.*Native$")
}

func TestColdCreateAttemptNativeFixture(t *testing.T) {
	coldCreateNativeFixture(t, "^TestColdCreateAttempt.*Native$")
}

func TestColdCreateAttemptCloseNativeFixture(t *testing.T) {
	coldCreateNativeFixture(t, "^TestColdCreateAttemptClosedBackendNative$")
}

func TestColdPreparationSettlementNativeFixture(t *testing.T) {
	coldCreateNativeFixture(t, "^TestColdPreparationSettlementNative$")
}

func coldCreateNativeFixture(t *testing.T, selector string) {
	if os.Getenv("SANDBOX_COLD_CREATE_NATIVE") != "1" {
		t.Skip("requires exact owned cold-create fixture")
	}
	dir := t.TempDir()
	admin, _, _ := observerNativeEtcd(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := admin.UserAddWithOptions(ctx, "server", "", &clientv3.UserAddOptions{NoPassword: true})
	require.NoError(t, err)
	_, err = admin.RoleAdd(ctx, "cold-create-write")
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "cold-create-write", "/sandbox/cold-create-native/", clientv3.GetPrefixRangeEnd("/sandbox/cold-create-native/"), clientv3.PermissionType(clientv3.PermReadWrite))
	require.NoError(t, err)
	_, err = admin.UserGrantRole(ctx, "server", "cold-create-write")
	require.NoError(t, err)
	child := exec.Command("/fixture/etcd-tests", "-test.run="+selector, "-test.v", "-test.timeout=60s")
	child.Env = append(os.Environ(), "COLD_CREATE_TLS_DIRECTORY="+dir)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	require.NoError(t, child.Start())
	err = child.Wait()
	t.Logf("original cold-create Backend test ELF Cmd.Wait=%v", err)
	require.NoError(t, err)
}
