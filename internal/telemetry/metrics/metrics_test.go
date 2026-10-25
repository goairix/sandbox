package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestFUSEMetricsInitialize(t *testing.T) {
	ResetForTest()
	require.NoError(t, InitNoop())
	require.NotNil(t, SandboxWorkspacePoolSize)
	require.NotNil(t, SandboxWorkspacePoolAcquire)
	require.NotNil(t, SandboxWorkspaceMountDuration)
	require.NotNil(t, SandboxWorkspaceLeaseLost)
	require.NotNil(t, SandboxWorkspacePoolCleanupTotal)
	require.NotNil(t, SandboxWorkspacePoolCleanupDuration)
	require.NotNil(t, SandboxWorkspaceStageDuration)
	require.NotNil(t, SandboxControllerStageDuration)
}

func TestControllerStageMetricUsesBoundedLabels(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previousMeter, previousHistogram := meter, SandboxControllerStageDuration
	meter = provider.Meter(t.Name())
	var err error
	SandboxControllerStageDuration, err = meter.Float64Histogram("sandbox.controller.stage.duration")
	require.NoError(t, err)
	t.Cleanup(func() {
		meter, SandboxControllerStageDuration = previousMeter, previousHistogram
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	RecordControllerStage(context.Background(), "renew", "grant", "error", 125*time.Millisecond)
	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &collected))
	require.Len(t, collected.ScopeMetrics, 1)
	require.Len(t, collected.ScopeMetrics[0].Metrics, 1)
	histogram, ok := collected.ScopeMetrics[0].Metrics[0].Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, histogram.DataPoints, 1)
	require.Equal(t, uint64(1), histogram.DataPoints[0].Count)
	require.InDelta(t, 0.125, histogram.DataPoints[0].Sum, 0.000001)
	require.ElementsMatch(t, []attribute.KeyValue{
		attribute.String("operation", "renew"),
		attribute.String("stage", "grant"),
		attribute.String("result", "error"),
	}, histogram.DataPoints[0].Attributes.ToSlice())
}
