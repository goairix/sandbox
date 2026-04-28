package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/telemetry"
)

var (
	meter metric.Meter

	// HTTP instruments
	HTTPRequestsTotal   metric.Int64Counter
	HTTPRequestDuration metric.Float64Histogram

	// Sandbox business instruments
	SandboxActiveGauge metric.Int64UpDownCounter
	SandboxExecTotal   metric.Int64Counter
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

	SandboxExecTotal, err = meter.Int64Counter(
		"sandbox.exec.total",
		metric.WithDescription("Total number of code executions"),
	)
	if err != nil {
		return fmt.Errorf("metrics: sandbox_exec_total: %w", err)
	}

	return nil
}

func Meter() metric.Meter {
	return meter
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
