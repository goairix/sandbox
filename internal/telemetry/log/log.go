package log

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/telemetry"
)

var otlpLoggerProvider *sdklog.LoggerProvider

func Init(cfg *config.Config) error {
	opts := []sdklog.LoggerProviderOption{
		sdklog.WithResource(telemetry.Resource()),
	}

	if cfg.Telemetry.Log.OtlpEnabled {
		if cfg.Telemetry.Log.OtlpEndpoint == "" {
			return fmt.Errorf("log: otlp_endpoint is empty")
		}
		exporter, err := otlploghttp.New(context.Background(),
			otlploghttp.WithEndpointURL(cfg.Telemetry.Log.OtlpEndpoint),
		)
		if err != nil {
			return fmt.Errorf("log: create otlp exporter: %w", err)
		}
		opts = append(opts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
	}

	otlpLoggerProvider = sdklog.NewLoggerProvider(opts...)
	global.SetLoggerProvider(otlpLoggerProvider)
	return nil
}

func Provider() *sdklog.LoggerProvider {
	return otlpLoggerProvider
}
