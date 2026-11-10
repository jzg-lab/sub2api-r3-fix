package httputil

import (
	"context"
	"errors"
	"testing"
)

func TestRequestBodyMemoryScopeReleasesOnlyItsBytes(t *testing.T) {
	if ctx, release := WithRequestBodyMemoryScope(context.Background()); ctx != context.Background() {
		t.Fatal("scope without a request budget must be a no-op")
	} else {
		release()
	}
	budget := NewBodyMemoryBudget(100)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	defer release()
	if err := ReserveRequestBodyMemory(ctx, 30); err != nil {
		t.Fatal(err)
	}
	scope, releaseScope := WithRequestBodyMemoryScope(ctx)
	if err := ReserveRequestBodyMemory(scope, 50); err != nil {
		t.Fatal(err)
	}
	if err := ReserveRequestBodyMemory(scope, 30); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("scope bypassed shared capacity: %v", err)
	}
	releaseScope()
	releaseScope() // Idempotent cleanup cannot refund the request's own bytes.
	if budget.used != 30 {
		t.Fatalf("scope release returned %d of the request's bytes", 30-budget.used)
	}
	if err := ReserveRequestBodyMemory(scope, 1); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("closed scope accepted a new reservation: %v", err)
	}
	release()
	late, releaseLate := WithRequestBodyMemoryScope(ctx)
	defer releaseLate()
	if err := ReserveRequestBodyMemory(late, 1); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("scope outlived its request: %v", err)
	}
	if budget.used != 0 {
		t.Fatalf("leaked reservations: %d", budget.used)
	}
}

func TestReleaseRequestBodyMemoryCapsAtOwnReservation(t *testing.T) {
	budget := NewBodyMemoryBudget(100)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	if err := ReserveRequestBodyMemory(ctx, 40); err != nil {
		t.Fatal(err)
	}
	scope, releaseScope := WithRequestBodyMemoryScope(ctx)
	defer releaseScope()
	if err := ReserveRequestBodyMemory(scope, 20); err != nil {
		t.Fatal(err)
	}
	ReleaseRequestBodyMemory(scope, 100)
	if budget.used != 40 {
		t.Fatalf("scope refunded another reservation: used=%d", budget.used)
	}
	ReleaseRequestBodyMemory(ctx, 10)
	ReleaseRequestBodyMemory(ctx, -1)
	if budget.used != 30 {
		t.Fatalf("partial release: used=%d", budget.used)
	}
	release()
	ReleaseRequestBodyMemory(ctx, 30)
	if budget.used != 0 {
		t.Fatalf("release after close changed the budget: used=%d", budget.used)
	}
}

func TestDetachRequestBodyMemoryOutlivesHandlerRelease(t *testing.T) {
	budget := NewBodyMemoryBudget(100)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	detached, releaseDetached, err := DetachRequestBodyMemory(ctx, 40)
	if err != nil {
		t.Fatal(err)
	}
	release() // The handler returns while the detached work still runs.
	if budget.used != 40 {
		t.Fatalf("detached body must stay reserved: %d", budget.used)
	}
	if err := ReserveRequestBodyMemory(detached, 30); err != nil {
		t.Fatalf("detached work must keep reserving after handler release: %v", err)
	}
	if err := ReserveRequestBodyMemory(detached, 31); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("detached reservations still share the budget: %v", err)
	}
	releaseDetached()
	releaseDetached()
	if budget.used != 0 {
		t.Fatalf("leaked reservations: %d", budget.used)
	}
}

func TestDetachRequestBodyMemoryTakesOverTheRequestCharge(t *testing.T) {
	budget := NewBodyMemoryBudget(100)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	if err := ReserveRequestBodyMemory(ctx, 70); err != nil {
		t.Fatal(err)
	}
	// The retained body is already charged to the request, so detaching it must
	// not need a second copy's worth of capacity.
	_, releaseDetached, err := DetachRequestBodyMemory(ctx, 60)
	if err != nil {
		t.Fatalf("detach charged the body twice: %v", err)
	}
	if budget.used != 70 {
		t.Fatalf("detach changed the shared usage: %d", budget.used)
	}
	release()
	if budget.used != 60 {
		t.Fatalf("handler release must leave only the detached body: %d", budget.used)
	}
	releaseDetached()
	if budget.used != 0 {
		t.Fatalf("leaked reservations: %d", budget.used)
	}

	ctx, release = WithRequestBodyMemoryBudget(context.Background(), budget)
	if err := ReserveRequestBodyMemory(ctx, 50); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DetachRequestBodyMemory(ctx, 101); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("want capacity error, got %v", err)
	}
	if budget.used != 50 {
		t.Fatalf("failed detach must return the body to the request: %d", budget.used)
	}
	release()
	if budget.used != 0 {
		t.Fatalf("failed detach leaked reservations: %d", budget.used)
	}
}

func TestDetachRequestBodyMemoryRejectsWhenBudgetExhausted(t *testing.T) {
	budget := NewBodyMemoryBudget(100)
	ctx, release := WithRequestBodyMemoryBudget(context.Background(), budget)
	defer release()
	if _, _, err := DetachRequestBodyMemory(ctx, 101); !errors.Is(err, ErrRequestBodyMemoryBudget) {
		t.Fatalf("want capacity error, got %v", err)
	}
	if budget.used != 0 {
		t.Fatalf("failed detach consumed budget: %d", budget.used)
	}
	if got, releaseNoop, err := DetachRequestBodyMemory(context.Background(), 101); err != nil || got == nil {
		t.Fatalf("without a budget detach is a no-op: %v", err)
	} else {
		releaseNoop()
	}
}
