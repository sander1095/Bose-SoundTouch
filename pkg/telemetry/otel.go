// Package telemetry initialises OpenTelemetry tracing, metrics, and logging
// for the SoundTouch Go binaries. Initialisation is a no-op when
// OTEL_EXPORTER_OTLP_ENDPOINT is not set, so the binaries run unchanged
// outside an Aspire / OTLP collector context.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// Shutdown stops every exporter the SDK created, flushing pending telemetry.
// Safe to call when initialisation was skipped — it returns nil.
type Shutdown func(context.Context) error

// Setup configures global tracer/meter/logger providers from environment
// variables when OTEL_EXPORTER_OTLP_ENDPOINT is set. Service name defaults
// to fallbackService when OTEL_SERVICE_NAME is not set.
func Setup(ctx context.Context, fallbackService string) (Shutdown, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = fallbackService
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithHost(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(15*time.Second))),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	if err := runtime.Start(runtime.WithMinimumReadMemStatsInterval(time.Second)); err != nil {
		log.Printf("otel: runtime metrics start failed: %v", err)
	}

	logExp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("otlp log exporter: %w", err)
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		sdklog.WithResource(res),
	)
	global.SetLoggerProvider(lp)

	// Install otelslog as the default slog handler. Code that calls
	// slog.InfoContext(ctx, …) / slog.ErrorContext(ctx, …) now gets
	// trace_id and span_id attached to every record — the ASP.NET-Core
	// "logs share their request's trace" experience for the slog API.
	// Non-context slog calls still ship records, just without correlation.
	slog.SetDefault(slog.New(otelslog.NewHandler(serviceName,
		otelslog.WithLoggerProvider(lp),
	)))

	// Tee stdlib log output so every existing log.Printf also flows to OTel.
	// Stdlib `log` has no context plumbing, so these records arrive without
	// trace_id — switch hot-path call sites to slog.InfoContext to get
	// correlation. Binaries that later override log.SetOutput should compose
	// LogSink() in their own MultiWriter to keep the sink in the chain.
	log.SetOutput(io.MultiWriter(log.Writer(), LogSink()))

	log.Printf("otel: telemetry initialised for service=%s endpoint=%s", serviceName, endpoint)

	return func(ctx context.Context) error {
		// Best-effort shutdown — flush each provider, collect errors.
		var firstErr error
		for _, fn := range []func(context.Context) error{
			tp.Shutdown, mp.Shutdown, lp.Shutdown,
		} {
			if err := fn(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}, nil
}
