package telemetry

import (
	"context"
	"errors"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Shutdown func(context.Context) error

func Configure(ctx context.Context, serviceName string) (Shutdown, error) {
	if !exportConfigured() || os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return func(context.Context) error { return nil }, nil
	}
	if serviceName == "" {
		serviceName = "llm-gateway"
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attribute.String("service.name", serviceName)),
	)
	if err != nil {
		return nil, err
	}
	genericEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
	var shutdowns []Shutdown
	if genericEndpoint || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		traceExporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		tracerProvider := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(traceExporter),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(tracerProvider)
		shutdowns = append(shutdowns, tracerProvider.Shutdown)
	}
	if genericEndpoint || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" {
		metricExporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			shutdownAll(ctx, shutdowns)
			return nil, err
		}
		meterProvider := metric.NewMeterProvider(
			metric.WithResource(res),
			metric.WithReader(metric.NewPeriodicReader(metricExporter, metric.WithInterval(15*time.Second))),
		)
		otel.SetMeterProvider(meterProvider)
		shutdowns = append(shutdowns, meterProvider.Shutdown)
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return func(shutdownCtx context.Context) error {
		return shutdownAll(shutdownCtx, shutdowns)
	}, nil
}

func shutdownAll(ctx context.Context, shutdowns []Shutdown) error {
	errorsToJoin := make([]error, 0, len(shutdowns))
	for index := len(shutdowns) - 1; index >= 0; index-- {
		errorsToJoin = append(errorsToJoin, shutdowns[index](ctx))
	}
	return errors.Join(errorsToJoin...)
}

func exportConfigured() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}
