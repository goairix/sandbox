//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"os"
	"testing"
)

func TestColdRuntimeQuotaReturnNativeFixture(t *testing.T) {
	if os.Getenv("SANDBOX_COLD_QUOTA_NATIVE") != "1" {
		t.Skip("requires exact owned cold quota return fixture")
	}
	coldCreateNativeFixture(t, "^TestColdRuntimeQuotaReturnNative$")
}
