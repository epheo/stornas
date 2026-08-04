package eventbus

import "testing"

// drained reports whether ch has a pending wake (and consumes it).
func drained(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestPublishWakesOnlySubscribersOfThatKind(t *testing.T) {
	b := New()
	live, _ := b.Subscribe(PoolChanged)
	git, _ := b.Subscribe(NodeChanged)

	b.Publish(NodeChanged)
	if drained(live) {
		t.Error("PoolChanged subscriber woke on a NodeChanged publish")
	}
	if !drained(git) {
		t.Error("NodeChanged subscriber did not wake on a NodeChanged publish")
	}
}

func TestSubscribeMultipleKinds(t *testing.T) {
	b := New()
	ch, _ := b.Subscribe(PoolChanged, VolumeChanged)

	b.Publish(VolumeChanged)
	if !drained(ch) {
		t.Error("did not wake on a subscribed kind (VolumeChanged)")
	}
	b.Publish(TargetChanged)
	if drained(ch) {
		t.Error("woke on an unsubscribed kind (TargetChanged)")
	}
}

func TestPublishCoalesces(t *testing.T) {
	b := New()
	ch, _ := b.Subscribe(PoolChanged)

	b.Publish(PoolChanged)
	b.Publish(PoolChanged)
	b.Publish(PoolChanged)
	if !drained(ch) {
		t.Fatal("expected a pending wake after publishes")
	}
	if drained(ch) {
		t.Error("expected coalescing to a single wake, found a second")
	}
}

func TestCancelUnsubscribes(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe(PoolChanged)
	cancel()
	b.Publish(PoolChanged)
	if drained(ch) {
		t.Error("a cancelled subscription still received a wake")
	}
	cancel() // second cancel must be a no-op, not a panic
}

func TestNilBusIsNoop(t *testing.T) {
	var b *Bus
	b.Publish(PoolChanged) // must not panic
	if v := b.Version(PoolChanged); v != 0 {
		t.Errorf("nil bus Version = %d, want 0", v)
	}
	ch, cancel := b.Subscribe(PoolChanged)
	if ch != nil {
		t.Error("nil bus Subscribe should yield a nil channel")
	}
	cancel() // must not panic
}

func TestVersionIncrementsPerKind(t *testing.T) {
	b := New()
	if v := b.Version(PoolChanged); v != 0 {
		t.Fatalf("fresh bus Version = %d, want 0", v)
	}
	b.Publish(PoolChanged)
	if v := b.Version(PoolChanged); v != 1 {
		t.Errorf("after one publish Version(PoolChanged) = %d, want 1", v)
	}
	// A different kind doesn't move this one.
	b.Publish(NodeChanged)
	if v := b.Version(PoolChanged); v != 1 {
		t.Errorf("Version(PoolChanged) moved on a NodeChanged publish: %d", v)
	}
	// The summed version of multiple kinds strictly increases when ANY moves.
	sum := b.Version(PoolChanged, NodeChanged)
	b.Publish(NodeChanged)
	if got := b.Version(PoolChanged, NodeChanged); got <= sum {
		t.Errorf("summed Version did not increase: %d <= %d", got, sum)
	}
}

// TestVersionBumpedBeforeWake is the load-bearing ordering guarantee: a consumer
// woken by a Publish must observe the incremented version (else it would reconcile
// against stale state and need an extra event to catch up).
func TestVersionBumpedBeforeWake(t *testing.T) {
	b := New()
	ch, _ := b.Subscribe(TargetChanged)
	before := b.Version(TargetChanged)
	b.Publish(TargetChanged)
	if !drained(ch) {
		t.Fatal("expected a wake")
	}
	// The wake fired, so by the bump-before-wake rule the version must already be ahead.
	if got := b.Version(TargetChanged); got <= before {
		t.Errorf("version not bumped before wake: %d <= %d", got, before)
	}
}
