package metrics

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	SandboxCoordinationStageDuration metric.Float64Histogram
	SandboxCoordinationGroups        metric.Int64UpDownCounter
	SandboxCoordinationFallbacks     metric.Int64Counter
	SandboxCoordinationWatchEvents   metric.Int64Counter
	SandboxCoordinationWatchBytes    metric.Int64Counter
)

func initCoordinationInstruments() (err error) {
	SandboxCoordinationStageDuration, err = meter.Float64Histogram("sandbox.coordination.stage.duration", metric.WithUnit("s"))
	if err != nil {
		return err
	}
	SandboxCoordinationGroups, err = meter.Int64UpDownCounter("sandbox.coordination.groups")
	if err != nil {
		return err
	}
	SandboxCoordinationFallbacks, err = meter.Int64Counter("sandbox.coordination.fallbacks")
	if err != nil {
		return err
	}
	SandboxCoordinationWatchEvents, err = meter.Int64Counter("sandbox.coordination.watch.events")
	if err != nil {
		return err
	}
	SandboxCoordinationWatchBytes, err = meter.Int64Counter("sandbox.coordination.watch.bytes", metric.WithUnit("By"))
	return err
}
func RecordCoordinationStage(ctx context.Context, operation, stage, result string, duration time.Duration) {
	if SandboxCoordinationStageDuration == nil {
		return
	}
	switch operation {
	case "open", "read", "renew", "end", "observe", "watch":
	default:
		return
	}
	switch stage {
	case "read", "grant", "commit", "keepalive", "revoke", "stream":
	default:
		return
	}
	switch result {
	case "success", "error", "conflict", "reset":
	default:
		return
	}
	SandboxCoordinationStageDuration.Record(ctx, duration.Seconds(), metric.WithAttributes(attribute.String("operation", operation), attribute.String("stage", stage), attribute.String("result", result)))
}
func RecordCoordinationGroup(ctx context.Context, delta int64) {
	if SandboxCoordinationGroups != nil {
		SandboxCoordinationGroups.Add(ctx, delta)
	}
}
func RecordCoordinationFallback(ctx context.Context, reason string) {
	if reason != "capacity" && reason != "unsupported" {
		return
	}
	if SandboxCoordinationFallbacks != nil {
		SandboxCoordinationFallbacks.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

// Counts include filtered prefix traffic so controller noise remains visible.
func RecordCoordinationWatch(ctx context.Context, events, bytes int64) {
	if SandboxCoordinationWatchEvents != nil {
		SandboxCoordinationWatchEvents.Add(ctx, events)
	}
	if SandboxCoordinationWatchBytes != nil {
		SandboxCoordinationWatchBytes.Add(ctx, bytes)
	}
}
