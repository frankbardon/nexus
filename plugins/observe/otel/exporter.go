package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// shutdownableProvider wraps a TracerProvider with its shutdown func.
type shutdownableProvider struct {
	provider trace.TracerProvider
	shutdown func(ctx context.Context) error
}

func newTracerProvider(ctx context.Context, cfg config) (*shutdownableProvider, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	var exporter sdktrace.SpanExporter

	switch cfg.protocol {
	case "http":
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(cfg.endpoint),
		}
		if cfg.insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
	default: // grpc
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.endpoint),
		}
		if cfg.insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	}

	if err != nil {
		return nil, fmt.Errorf("otel exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	return &shutdownableProvider{
		provider: tp,
		shutdown: tp.Shutdown,
	}, nil
}
