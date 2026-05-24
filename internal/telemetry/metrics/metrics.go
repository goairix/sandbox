package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/telemetry"
)

var (
	meter metric.Meter

	// HTTP instruments
	HTTPRequestsTotal   metric.Int64Counter
	HTTPRequestDuration metric.Float64Histogram

	// Sandbox lifecycle instruments
	SandboxActiveGauge    metric.Int64UpDownCounter
	SandboxCreateTotal    metric.Int64Counter
	SandboxCreateDuration metric.Float64Histogram
	SandboxDestroyTotal   metric.Int64Counter

	// Dependency install (a common cause of slow Create)
	SandboxDependencyInstallDuration metric.Float64Histogram

	// Exec instruments
	SandboxExecTotal    metric.Int64Counter
	SandboxExecDuration metric.Float64Histogram

	// Pool instruments
	SandboxPoolSize           metric.Int64UpDownCounter
	SandboxPoolHitTotal       metric.Int64Counter
	SandboxPoolRefillFailures metric.Int64Counter

	// File operation instruments
	SandboxFileOpTotal metric.Int64Counter

	// Workspace sync instruments
	SandboxWorkspaceSyncTotal    metric.Int64Counter
	SandboxWorkspaceSyncDuration metric.Float64Histogram

	// Session restore (persistent sandbox restore on startup / multi-replica)
	SandboxSessionRestoreTotal metric.Int64Counter

	// Error instruments
	SandboxErrorTotal metric.Int64Counter
)

// Init 初始化指标 meterProvider
func Init(cfg *config.Config) error {
	mpOpts := []sdkmetric.Option{
		sdkmetric.WithResource(telemetry.Resource()),
	}

	if cfg.Telemetry.Metrics.OtlpEnabled {
		if cfg.Telemetry.Metrics.OtlpEndpoint == "" {
			return fmt.Errorf("metrics: otlp_endpoint is empty")
		}
		exp, err := otlpmetrichttp.New(
			context.Background(),
			otlpmetrichttp.WithEndpointURL(cfg.Telemetry.Metrics.OtlpEndpoint),
		)
		if err != nil {
			return fmt.Errorf("metrics: create otlp exporter: %w", err)
		}
		mpOpts = append(mpOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(15*time.Second)),
		))
	}

	meterProvider := sdkmetric.NewMeterProvider(mpOpts...)
	otel.SetMeterProvider(meterProvider)

	meter = otel.Meter(cfg.Telemetry.ServiceName)

	return initInstruments()
}

func initInstruments() (err error) {
	HTTPRequestsTotal, err = meter.Int64Counter(
		"http.server.requests.total",
		metric.WithDescription("Total number of HTTP requests"),
	)
	if err != nil {
		return fmt.Errorf("metrics: http_requests_total: %w", err)
	}

	HTTPRequestDuration, err = meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithDescription("HTTP request duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		return fmt.Errorf("metrics: http_request_duration: %w", err)
	}

	SandboxActiveGauge, err = meter.Int64UpDownCounter(
		"sandbox.active",
		metric.WithDescription("Number of currently active sandboxes"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_active: %w", err)
	}

	SandboxCreateTotal, err = meter.Int64Counter(
		"sandbox.create.total",
		metric.WithDescription("Total number of sandbox creations"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_create_total: %w", err)
	}

	SandboxCreateDuration, err = meter.Float64Histogram(
		"sandbox.create.duration",
		metric.WithDescription("Sandbox creation duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_create_duration: %w", err)
	}

	SandboxDestroyTotal, err = meter.Int64Counter(
		"sandbox.destroy.total",
		metric.WithDescription("Total number of sandbox destructions"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_destroy_total: %w", err)
	}

	SandboxDependencyInstallDuration, err = meter.Float64Histogram(
		"sandbox.dependency.install.duration",
		metric.WithDescription("Duration of dependency install during sandbox creation"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 20, 30, 60, 120),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_dependency_install_duration: %w", err)
	}

	SandboxExecTotal, err = meter.Int64Counter(
		"sandbox.exec.total",
		metric.WithDescription("Total number of code executions"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_exec_total: %w", err)
	}

	SandboxExecDuration, err = meter.Float64Histogram(
		"sandbox.exec.duration",
		metric.WithDescription("Sandbox execution duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_exec_duration: %w", err)
	}

	SandboxPoolSize, err = meter.Int64UpDownCounter(
		"sandbox.pool.size",
		metric.WithDescription("Number of warm containers in the pool"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_pool_size: %w", err)
	}

	SandboxPoolHitTotal, err = meter.Int64Counter(
		"sandbox.pool.hit.total",
		metric.WithDescription("Total number of pool acquire attempts"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_pool_hit_total: %w", err)
	}

	SandboxPoolRefillFailures, err = meter.Int64Counter(
		"sandbox.pool.refill.failures.total",
		metric.WithDescription("Total number of pool refill failures"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_pool_refill_failures: %w", err)
	}

	SandboxFileOpTotal, err = meter.Int64Counter(
		"sandbox.file.operation.total",
		metric.WithDescription("Total number of file operations"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_file_op_total: %w", err)
	}

	SandboxWorkspaceSyncTotal, err = meter.Int64Counter(
		"sandbox.workspace.sync.total",
		metric.WithDescription("Total number of workspace sync operations"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_workspace_sync_total: %w", err)
	}

	SandboxWorkspaceSyncDuration, err = meter.Float64Histogram(
		"sandbox.workspace.sync.duration",
		metric.WithDescription("Workspace sync duration in seconds"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_workspace_sync_duration: %w", err)
	}

	SandboxSessionRestoreTotal, err = meter.Int64Counter(
		"sandbox.session.restore.total",
		metric.WithDescription("Total number of persistent sandbox restorations"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_session_restore_total: %w", err)
	}

	SandboxErrorTotal, err = meter.Int64Counter(
		"sandbox.error.total",
		metric.WithDescription("Total number of sandbox business errors"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_error_total: %w", err)
	}

	return nil
}

func Meter() metric.Meter {
	return meter
}

// InitNoop initialises all instruments with a no-op provider.
// Call this from TestMain in packages that use metrics but don't need real export.
func InitNoop() error {
	otel.SetMeterProvider(noop.NewMeterProvider())
	meter = otel.Meter("noop")
	return initInstruments()
}

// RecordHTTP records HTTP request count and duration.
func RecordHTTP(ctx context.Context, method, route string, statusCode int, duration float64) {
	attrs := []attribute.KeyValue{
		attribute.String("http.method", method),
		attribute.String("http.route", route),
		attribute.Int("http.status_code", statusCode),
	}
	HTTPRequestsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	HTTPRequestDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
}

// RecordSandboxCreate records a sandbox creation attempt.
// source: "pool" or "direct"; status: "success" or "error".
func RecordSandboxCreate(ctx context.Context, source, status string, duration float64) {
	attrs := []attribute.KeyValue{
		attribute.String("source", source),
		attribute.String("status", status),
	}
	SandboxCreateTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	if status == "success" {
		SandboxCreateDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
	}
}

// RecordSandboxDestroy records a sandbox destruction.
// reason: "manual" or "ttl_expired".
func RecordSandboxDestroy(ctx context.Context, reason string) {
	SandboxDestroyTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", reason),
	))
}

// RecordDependencyInstall records dependency install duration during sandbox creation.
// status: "success" or "error".
func RecordDependencyInstall(ctx context.Context, status string, duration float64) {
	SandboxDependencyInstallDuration.Record(ctx, duration, metric.WithAttributes(
		attribute.String("status", status),
	))
}

// RecordExec records an execution attempt.
// status: "success" (exit_code == 0), "non_zero_exit" (process exited with non-zero), or "error" (transport/runtime error).
// kind: "sync" or "stream".
func RecordExec(ctx context.Context, kind, status string, duration float64) {
	attrs := []attribute.KeyValue{
		attribute.String("kind", kind),
		attribute.String("status", status),
	}
	SandboxExecTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	SandboxExecDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
}

// RecordPoolAcquire records a pool acquire attempt.
// hit=true means a warm container was successfully reused; false means an on-demand
// container was created (either pool empty or all warm containers were stale).
func RecordPoolAcquire(ctx context.Context, hit bool) {
	SandboxPoolHitTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.Bool("hit", hit),
	))
}

// RecordPoolRefillFailure records a single failed pool refill attempt.
func RecordPoolRefillFailure(ctx context.Context) {
	SandboxPoolRefillFailures.Add(ctx, 1)
}

// RecordFileOp records a file operation.
// operation: "upload", "download", "read", "read_lines", "edit", "edit_lines".
// status: "success" or "error".
func RecordFileOp(ctx context.Context, operation, status string) {
	SandboxFileOpTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.String("status", status),
	))
}

// RecordWorkspaceSync records a workspace sync operation.
// direction: "to_container" or "from_container"; status: "success" or "error".
func RecordWorkspaceSync(ctx context.Context, direction, status string, duration float64) {
	attrs := []attribute.KeyValue{
		attribute.String("direction", direction),
		attribute.String("status", status),
	}
	SandboxWorkspaceSyncTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	SandboxWorkspaceSyncDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
}

// RecordSessionRestore records a persistent sandbox restoration.
// status: "success" or "error".
func RecordSessionRestore(ctx context.Context, status string) {
	SandboxSessionRestoreTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("status", status),
	))
}

// RecordError records a sandbox business error.
func RecordError(ctx context.Context, errType string) {
	SandboxErrorTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("type", errType),
	))
}
