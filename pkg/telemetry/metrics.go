package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RegisterDeviceCountGauge wires an observable gauge that reports the
// current number of devices known to the datastore. The callback fn runs
// on the SDK's collection interval — keep it cheap. Returns a no-op
// registration when telemetry is disabled, so callers don't need to guard.
func RegisterDeviceCountGauge(name string, count func(context.Context) (int64, error)) error {
	meter := otel.Meter("github.com/gesellix/bose-soundtouch/pkg/telemetry")

	gauge, err := meter.Int64ObservableGauge(name,
		metric.WithDescription("Current number of SoundTouch devices known to the service."),
	)
	if err != nil {
		return fmt.Errorf("create gauge %s: %w", name, err)
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			n, cbErr := count(ctx)
			if cbErr != nil {
				return cbErr
			}
			o.ObserveInt64(gauge, n, metric.WithAttributes(
				attribute.String("source", "datastore"),
			))
			return nil
		},
		gauge,
	)
	if err != nil {
		return fmt.Errorf("register callback for %s: %w", name, err)
	}
	return nil
}
