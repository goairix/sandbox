package sandbox

import (
	"os"
	"testing"

	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

func TestMain(m *testing.M) {
	if err := metrics.InitNoop(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
