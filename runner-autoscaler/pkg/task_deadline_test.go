package pkg

import (
	"context"
	"testing"
	"time"
)

// Cloud Tasks rejects any API request whose gRPC deadline is more than 30s in the
// future ("The deadline cannot be more than 30s in the future"). The recreate path
// enqueues the create-vm callback on the 180s opContext, so without bounding the
// context every CreateTask in that path fails with InvalidArgument and the job that
// the dead VM was supposed to run never gets a replacement runner.
const cloudTasksMaxFutureDeadline = 30 * time.Second

// A long-lived parent context (e.g. the 180s opContext) must be capped so the
// deadline propagated to the Cloud Tasks RPC is under the 30s server limit.
func TestBoundTaskRPCContext_CapsLongDeadline(t *testing.T) {

	parent, cancelParent := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancelParent()

	ctx, cancel := boundTaskRPCContext(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected the bounded context to carry a deadline")
	}
	remaining := time.Until(deadline)
	if remaining > cloudTasksMaxFutureDeadline {
		t.Fatalf("bounded deadline is %v in the future - Cloud Tasks rejects anything over %v", remaining, cloudTasksMaxFutureDeadline)
	}
	// Guard against collapsing the budget to ~0, which would make the RPC time out
	// before it can complete (and defeat the retry logic inside CreateTask).
	if remaining < 15*time.Second {
		t.Fatalf("bounded deadline is only %v in the future - too short to complete the enqueue", remaining)
	}
}

// A context that already has a deadline earlier than the cap must keep its own,
// earlier deadline (we only ever shorten, never extend).
func TestBoundTaskRPCContext_PreservesEarlierDeadline(t *testing.T) {

	parent, cancelParent := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelParent()

	ctx, cancel := boundTaskRPCContext(parent)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected the bounded context to carry a deadline")
	}
	if remaining := time.Until(deadline); remaining > 6*time.Second {
		t.Fatalf("earlier parent deadline was not preserved: %v remaining", remaining)
	}
}

// A context with no deadline (e.g. context.Background()) must gain the cap so the
// propagated Cloud Tasks deadline is bounded.
func TestBoundTaskRPCContext_BoundsDeadlinelessContext(t *testing.T) {

	ctx, cancel := boundTaskRPCContext(context.Background())
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline to be imposed on a deadline-less parent")
	}
	if remaining := time.Until(deadline); remaining > cloudTasksMaxFutureDeadline {
		t.Fatalf("imposed deadline is %v in the future - Cloud Tasks rejects anything over %v", remaining, cloudTasksMaxFutureDeadline)
	}
}
