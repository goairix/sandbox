package trace

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/telemetry"
)

var provider *sdktrace.TracerProvider
var tracer trace.Tracer

func Init(cfg *config.Config) error {
	tpOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(telemetry.Resource()),
	}

	if cfg.Telemetry.Tracer.OtlpEnabled {
		if cfg.Telemetry.Tracer.OtlpEndpoint == "" {
			return fmt.Errorf("tracer: otlp_endpoint is empty")
		}
		exp, err := otlptracehttp.New(
			context.Background(),
			otlptracehttp.WithEndpointURL(cfg.Telemetry.Tracer.OtlpEndpoint),
		)
		if err != nil {
			return fmt.Errorf("tracer: create otlp exporter: %w", err)
		}
		tpOpts = append(tpOpts, sdktrace.WithBatcher(exp))
	}

	provider = sdktrace.NewTracerProvider(tpOpts...)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tracer = NewTracer(
		telemetry.ServiceName(cfg),
		trace.WithInstrumentationVersion(cfg.Telemetry.ServiceVersion),
	)

	return nil
}

func TracerProvider() *sdktrace.TracerProvider {
	return provider
}

func NewTracer(traceName string, opts ...trace.TracerOption) trace.Tracer {
	return provider.Tracer(traceName, opts...)
}

func Tracer() trace.Tracer {
	return tracer
}

// ParseContextTraceId 从上下文获取 traceId
func ParseContextTraceId(ctx context.Context) string {
	if v, ok := ctx.Value("X-Trace-Id").(string); ok && v != "" {
		return v
	}
	span := trace.SpanFromContext(ctx)
	if span.SpanContext().HasTraceID() {
		return span.SpanContext().TraceID().String()
	}
	return ""
}

func Gin(ctx *gin.Context) context.Context {
	return ctx.Request.Context()
}
