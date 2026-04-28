package telemetry

import (
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"

	"github.com/goairix/sandbox/internal/config"
)

var globalResource *resource.Resource

func Init(cfg *config.Config) error {
	var err error
	globalResource, err = resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(ServiceName(cfg)),
			semconv.ServiceVersion(cfg.Telemetry.ServiceVersion),
		),
	)
	return err
}

func Resource() *resource.Resource {
	return globalResource
}

func ServiceName(cfg *config.Config) string {
	return cfg.Telemetry.ServiceName
}
