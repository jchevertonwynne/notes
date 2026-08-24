package main

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
)

// storeTracer is named after the conceptual package Store belongs to — this
// app has no separate internal/store, but the naming convention (tracer
// named after the code it instruments, not the service) is worth keeping
// anyway.
var storeTracer = otel.Tracer("notes/store")

// withSpan runs fn inside a span named "notes.<op>", recording fn's error
// onto the span before returning it.
func withSpan(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	ctx, span := storeTracer.Start(ctx, "notes."+op)
	defer span.End()
	err := fn(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}
