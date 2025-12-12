// Package retry provides retry policies for activities.
package retry

import (
	"errors"
	"math"
	"math/rand"
	"time"
)

// Policy defines the retry behavior for activities.
type Policy struct {
	// MaxAttempts is the maximum number of attempts (including the first).
	// 0 means no limit.
	MaxAttempts int

	// InitialInterval is the delay before the first retry.
	InitialInterval time.Duration

	// MaxInterval is the maximum delay between retries.
	MaxInterval time.Duration

	// Multiplier is the factor by which the interval increases.
	Multiplier float64

	// RandomizationFactor adds jitter to the delay.
	// A value of 0.5 means the actual delay will be within [delay * 0.5, delay * 1.5].
	RandomizationFactor float64

	// NonRetryableErrors is a list of errors that should not be retried.
	// Errors are matched using errors.Is.
	NonRetryableErrors []error
}

// DefaultPolicy returns a sensible default retry policy.
func DefaultPolicy() *Policy {
	return &Policy{
		MaxAttempts:         3,
		InitialInterval:     100 * time.Millisecond,
		MaxInterval:         10 * time.Second,
		Multiplier:          2.0,
		RandomizationFactor: 0.5,
	}
}

// NoRetry returns a policy that never retries.
func NoRetry() *Policy {
	return &Policy{
		MaxAttempts: 1,
	}
}

// Fixed returns a policy with fixed delay between retries.
func Fixed(maxAttempts int, interval time.Duration) *Policy {
	return &Policy{
		MaxAttempts:     maxAttempts,
		InitialInterval: interval,
		MaxInterval:     interval,
		Multiplier:      1.0,
	}
}

// Exponential returns an exponential backoff policy.
func Exponential(maxAttempts int, initial, max time.Duration, multiplier float64) *Policy {
	return &Policy{
		MaxAttempts:         maxAttempts,
		InitialInterval:     initial,
		MaxInterval:         max,
		Multiplier:          multiplier,
		RandomizationFactor: 0.5,
	}
}

// ShouldRetry returns true if the operation should be retried.
func (p *Policy) ShouldRetry(attempts int, err error) bool {
	if p.MaxAttempts > 0 && attempts >= p.MaxAttempts {
		return false
	}

	// Check if error is in the non-retryable list
	for _, nonRetryable := range p.NonRetryableErrors {
		if errors.Is(err, nonRetryable) {
			return false
		}
	}

	return true
}

// GetDelay calculates the delay before the next retry.
func (p *Policy) GetDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return p.addJitter(p.InitialInterval)
	}

	// Calculate exponential backoff
	delay := float64(p.InitialInterval) * math.Pow(p.Multiplier, float64(attempts-1))

	// Cap at maximum interval
	if delay > float64(p.MaxInterval) {
		delay = float64(p.MaxInterval)
	}

	return p.addJitter(time.Duration(delay))
}

// addJitter adds randomization to the delay.
func (p *Policy) addJitter(delay time.Duration) time.Duration {
	if p.RandomizationFactor == 0 {
		return delay
	}

	// Generate random factor between [1-factor, 1+factor]
	factor := 1.0 + p.RandomizationFactor*(2*rand.Float64()-1)
	return time.Duration(float64(delay) * factor)
}

// Builder provides a fluent API for building policies.
type Builder struct {
	policy *Policy
}

// NewBuilder creates a new policy builder.
func NewBuilder() *Builder {
	return &Builder{
		policy: &Policy{
			MaxAttempts:     3,
			InitialInterval: 100 * time.Millisecond,
			MaxInterval:     10 * time.Second,
			Multiplier:      2.0,
		},
	}
}

// WithMaxAttempts sets the maximum number of attempts.
func (b *Builder) WithMaxAttempts(n int) *Builder {
	b.policy.MaxAttempts = n
	return b
}

// WithInitialInterval sets the initial retry interval.
func (b *Builder) WithInitialInterval(d time.Duration) *Builder {
	b.policy.InitialInterval = d
	return b
}

// WithMaxInterval sets the maximum retry interval.
func (b *Builder) WithMaxInterval(d time.Duration) *Builder {
	b.policy.MaxInterval = d
	return b
}

// WithMultiplier sets the backoff multiplier.
func (b *Builder) WithMultiplier(m float64) *Builder {
	b.policy.Multiplier = m
	return b
}

// WithJitter sets the randomization factor.
func (b *Builder) WithJitter(factor float64) *Builder {
	b.policy.RandomizationFactor = factor
	return b
}

// WithNonRetryableErrors sets errors that should not trigger retries.
func (b *Builder) WithNonRetryableErrors(errs ...error) *Builder {
	b.policy.NonRetryableErrors = errs
	return b
}

// Build returns the configured policy.
func (b *Builder) Build() *Policy {
	return b.policy
}
