package middleware

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

// OTel returns a Gin middleware that:
//   - extracts incoming trace context from request headers (W3C TraceContext)
//   - falls back to X-Trace-Id header for upstream clients that don't use W3C
//   - starts a server span for each request
//   - injects the span into the request context so downstream handlers can use it
//   - records HTTP status code and marks the span as error on 5xx
func OTel(serviceName string) gin.HandlerFunc {
	tracer := otel.Tracer(serviceName)
	propagator := otel.GetTextMapPropagator()

	return func(c *gin.Context) {
		// Extract upstream trace context from headers (W3C traceparent / tracestate)
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))

		// If no valid remote span was found via W3C, try X-Trace-Id + X-Span-Id
		if !trace.SpanFromContext(ctx).SpanContext().IsRemote() {
			if xTraceID := c.GetHeader("X-Trace-Id"); xTraceID != "" {
				if traceID, err := parseTraceID(xTraceID); err == nil {
					spanCfg := trace.SpanContextConfig{
						TraceID:    traceID,
						TraceFlags: trace.FlagsSampled,
						Remote:     true,
					}
					if xSpanID := c.GetHeader("X-Span-Id"); xSpanID != "" {
						if spanID, err := parseSpanID(xSpanID); err == nil {
							spanCfg.SpanID = spanID
						}
					}
					sc := trace.NewSpanContext(spanCfg)
					ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
				}
			}
		}

		// Span name: "HTTP METHOD /path/template"
		spanName := fmt.Sprintf("%s %s", c.Request.Method, c.FullPath())
		if spanName == " " {
			spanName = fmt.Sprintf("%s %s", c.Request.Method, c.Request.URL.Path)
		}

		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPRequestMethodKey.String(c.Request.Method),
				semconv.URLPath(c.Request.URL.Path),
				semconv.ServerAddress(c.Request.Host),
				attribute.String("http.route", c.FullPath()),
			),
		)
		defer span.End()

		// Replace request context so handlers downstream get the span
		c.Request = c.Request.WithContext(ctx)

		start := time.Now()
		c.Next()
		elapsed := time.Since(start).Seconds()

		status := c.Writer.Status()
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path
		}

		span.SetAttributes(semconv.HTTPResponseStatusCode(status))

		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		} else {
			span.SetStatus(codes.Ok, "")
		}

		// Record HTTP metrics
		metrics.RecordHTTP(ctx, c.Request.Method, route, status, elapsed)

		// Propagate trace context to response headers so clients can correlate
		propagator.Inject(ctx, propagation.HeaderCarrier(c.Writer.Header()))
	}
}

// parseTraceID parses a 32-char hex trace ID string into a trace.TraceID.
func parseTraceID(s string) (trace.TraceID, error) {
	var id trace.TraceID
	if len(s) != 32 {
		return id, fmt.Errorf("invalid trace id length: %d", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}

// parseSpanID parses a 16-char hex span ID string into a trace.SpanID.
func parseSpanID(s string) (trace.SpanID, error) {
	var id trace.SpanID
	if len(s) != 16 {
		return id, fmt.Errorf("invalid span id length: %d", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	copy(id[:], b)
	return id, nil
}
