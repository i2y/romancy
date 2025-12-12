package retry

import (
	"errors"
	"testing"
	"time"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("unauthorized")
	ErrInternal     = errors.New("internal error")
)

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()

	if p.MaxAttempts != 3 {
		t.Errorf("expected MaxAttempts=3, got %d", p.MaxAttempts)
	}
	if p.InitialInterval != 100*time.Millisecond {
		t.Errorf("expected InitialInterval=100ms, got %v", p.InitialInterval)
	}
	if p.MaxInterval != 10*time.Second {
		t.Errorf("expected MaxInterval=10s, got %v", p.MaxInterval)
	}
	if p.Multiplier != 2.0 {
		t.Errorf("expected Multiplier=2.0, got %f", p.Multiplier)
	}
	if p.RandomizationFactor != 0.5 {
		t.Errorf("expected RandomizationFactor=0.5, got %f", p.RandomizationFactor)
	}
}

func TestNoRetry(t *testing.T) {
	p := NoRetry()

	if p.MaxAttempts != 1 {
		t.Errorf("expected MaxAttempts=1, got %d", p.MaxAttempts)
	}
	if p.ShouldRetry(1, ErrInternal) {
		t.Error("expected ShouldRetry to return false after 1 attempt")
	}
}

func TestFixed(t *testing.T) {
	p := Fixed(5, 200*time.Millisecond)

	if p.MaxAttempts != 5 {
		t.Errorf("expected MaxAttempts=5, got %d", p.MaxAttempts)
	}
	if p.InitialInterval != 200*time.Millisecond {
		t.Errorf("expected InitialInterval=200ms, got %v", p.InitialInterval)
	}
	if p.MaxInterval != 200*time.Millisecond {
		t.Errorf("expected MaxInterval=200ms, got %v", p.MaxInterval)
	}
	if p.Multiplier != 1.0 {
		t.Errorf("expected Multiplier=1.0, got %f", p.Multiplier)
	}
}

func TestExponential(t *testing.T) {
	p := Exponential(10, 100*time.Millisecond, 5*time.Second, 1.5)

	if p.MaxAttempts != 10 {
		t.Errorf("expected MaxAttempts=10, got %d", p.MaxAttempts)
	}
	if p.InitialInterval != 100*time.Millisecond {
		t.Errorf("expected InitialInterval=100ms, got %v", p.InitialInterval)
	}
	if p.MaxInterval != 5*time.Second {
		t.Errorf("expected MaxInterval=5s, got %v", p.MaxInterval)
	}
	if p.Multiplier != 1.5 {
		t.Errorf("expected Multiplier=1.5, got %f", p.Multiplier)
	}
}

func TestShouldRetry_MaxAttempts(t *testing.T) {
	p := &Policy{MaxAttempts: 3}

	// Should retry for attempts < MaxAttempts
	if !p.ShouldRetry(1, ErrInternal) {
		t.Error("expected ShouldRetry to return true for attempt 1")
	}
	if !p.ShouldRetry(2, ErrInternal) {
		t.Error("expected ShouldRetry to return true for attempt 2")
	}
	// Should not retry when attempts >= MaxAttempts
	if p.ShouldRetry(3, ErrInternal) {
		t.Error("expected ShouldRetry to return false for attempt 3")
	}
	if p.ShouldRetry(4, ErrInternal) {
		t.Error("expected ShouldRetry to return false for attempt 4")
	}
}

func TestShouldRetry_UnlimitedAttempts(t *testing.T) {
	p := &Policy{MaxAttempts: 0} // 0 means unlimited

	// Should always retry when MaxAttempts is 0
	for i := 1; i <= 100; i++ {
		if !p.ShouldRetry(i, ErrInternal) {
			t.Errorf("expected ShouldRetry to return true for attempt %d with unlimited retries", i)
		}
	}
}

func TestShouldRetry_NonRetryableErrors(t *testing.T) {
	p := &Policy{
		MaxAttempts:        10,
		NonRetryableErrors: []error{ErrNotFound, ErrUnauthorized},
	}

	// Non-retryable errors should not be retried
	if p.ShouldRetry(1, ErrNotFound) {
		t.Error("expected ShouldRetry to return false for ErrNotFound")
	}
	if p.ShouldRetry(1, ErrUnauthorized) {
		t.Error("expected ShouldRetry to return false for ErrUnauthorized")
	}

	// Other errors should be retried
	if !p.ShouldRetry(1, ErrInternal) {
		t.Error("expected ShouldRetry to return true for ErrInternal")
	}
}

func TestShouldRetry_WrappedNonRetryableErrors(t *testing.T) {
	p := &Policy{
		MaxAttempts:        10,
		NonRetryableErrors: []error{ErrNotFound},
	}

	// Wrapped errors should also match using errors.Is
	wrappedErr := errors.Join(errors.New("wrapper"), ErrNotFound)
	if p.ShouldRetry(1, wrappedErr) {
		t.Error("expected ShouldRetry to return false for wrapped ErrNotFound")
	}
}

func TestGetDelay_ExponentialBackoff(t *testing.T) {
	p := &Policy{
		InitialInterval:     100 * time.Millisecond,
		MaxInterval:         10 * time.Second,
		Multiplier:          2.0,
		RandomizationFactor: 0, // No jitter for predictable tests
	}

	// First attempt should use initial interval
	delay := p.GetDelay(1)
	if delay != 100*time.Millisecond {
		t.Errorf("expected delay=100ms for attempt 1, got %v", delay)
	}

	// Second attempt: initial * 2^1 = 200ms
	delay = p.GetDelay(2)
	if delay != 200*time.Millisecond {
		t.Errorf("expected delay=200ms for attempt 2, got %v", delay)
	}

	// Third attempt: initial * 2^2 = 400ms
	delay = p.GetDelay(3)
	if delay != 400*time.Millisecond {
		t.Errorf("expected delay=400ms for attempt 3, got %v", delay)
	}
}

func TestGetDelay_MaxInterval(t *testing.T) {
	p := &Policy{
		InitialInterval:     1 * time.Second,
		MaxInterval:         5 * time.Second,
		Multiplier:          2.0,
		RandomizationFactor: 0,
	}

	// Delay should be capped at MaxInterval
	// Attempt 4: 1s * 2^3 = 8s, but capped at 5s
	delay := p.GetDelay(4)
	if delay != 5*time.Second {
		t.Errorf("expected delay=5s (capped) for attempt 4, got %v", delay)
	}
}

func TestGetDelay_WithJitter(t *testing.T) {
	p := &Policy{
		InitialInterval:     100 * time.Millisecond,
		MaxInterval:         10 * time.Second,
		Multiplier:          2.0,
		RandomizationFactor: 0.5,
	}

	// Run multiple times to check jitter is applied
	minDelay := 50 * time.Millisecond  // 100ms * 0.5
	maxDelay := 150 * time.Millisecond // 100ms * 1.5

	for i := 0; i < 100; i++ {
		delay := p.GetDelay(1)
		if delay < minDelay || delay > maxDelay {
			t.Errorf("expected delay between %v and %v, got %v", minDelay, maxDelay, delay)
		}
	}
}

func TestBuilder(t *testing.T) {
	p := NewBuilder().
		WithMaxAttempts(5).
		WithInitialInterval(500*time.Millisecond).
		WithMaxInterval(30*time.Second).
		WithMultiplier(1.5).
		WithJitter(0.3).
		WithNonRetryableErrors(ErrNotFound, ErrUnauthorized).
		Build()

	if p.MaxAttempts != 5 {
		t.Errorf("expected MaxAttempts=5, got %d", p.MaxAttempts)
	}
	if p.InitialInterval != 500*time.Millisecond {
		t.Errorf("expected InitialInterval=500ms, got %v", p.InitialInterval)
	}
	if p.MaxInterval != 30*time.Second {
		t.Errorf("expected MaxInterval=30s, got %v", p.MaxInterval)
	}
	if p.Multiplier != 1.5 {
		t.Errorf("expected Multiplier=1.5, got %f", p.Multiplier)
	}
	if p.RandomizationFactor != 0.3 {
		t.Errorf("expected RandomizationFactor=0.3, got %f", p.RandomizationFactor)
	}
	if len(p.NonRetryableErrors) != 2 {
		t.Errorf("expected 2 NonRetryableErrors, got %d", len(p.NonRetryableErrors))
	}

	// Verify non-retryable errors work
	if p.ShouldRetry(1, ErrNotFound) {
		t.Error("expected ShouldRetry to return false for ErrNotFound")
	}
}
