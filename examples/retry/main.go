// Package main demonstrates retry policies in activities.
//
// This example shows:
// - Exponential backoff with jitter
// - Non-retryable errors
// - Custom retry policies
// - Activity timeout handling
//
// Usage:
//
//	go run ./examples/retry/
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/hooks/otel"
	"github.com/i2y/romancy/retry"
)

// ----- Types -----

// APICallInput is the input for the API call workflow.
type APICallInput struct {
	Endpoint   string `json:"endpoint"`
	MaxRetries int    `json:"max_retries"`
}

// APICallResult is the output of the API call workflow.
type APICallResult struct {
	Endpoint     string `json:"endpoint"`
	ResponseCode int    `json:"response_code"`
	Attempts     int    `json:"attempts"`
	Status       string `json:"status"`
}

// ----- Custom Errors -----

// ErrRateLimited indicates the API returned 429 Too Many Requests.
var ErrRateLimited = errors.New("rate limited")

// ErrBadRequest indicates a 400 Bad Request (non-retryable).
var ErrBadRequest = errors.New("bad request")

// ErrNotFound indicates a 404 Not Found (non-retryable).
var ErrNotFound = errors.New("not found")

// ErrServerError indicates a 5xx server error.
var ErrServerError = errors.New("server error")

// ----- Activities -----

// Shared counters for demonstrating retries
var (
	flakyCallAttempts   int64
	failingCallAttempts int64
)

// FlakyAPICall simulates an unreliable API that sometimes fails.
var FlakyAPICall = romancy.DefineActivity(
	"flaky_api_call",
	func(ctx context.Context, endpoint string) (int, error) {
		attempt := atomic.AddInt64(&flakyCallAttempts, 1)
		log.Printf("[Activity] Calling flaky API: %s (attempt %d)", endpoint, attempt)

		// Simulate random failures (50% chance of failure for first few attempts)
		if attempt < 3 && rand.Float64() < 0.7 {
			log.Printf("[Activity] API call failed (transient error)")
			return 0, fmt.Errorf("connection timeout: %w", ErrServerError)
		}

		// Success
		log.Printf("[Activity] API call succeeded!")
		return 200, nil
	},
	// Exponential backoff: 100ms, 200ms, 400ms, 800ms...
	romancy.WithRetryPolicy[string, int](retry.Exponential(
		5,                    // Max 5 attempts
		100*time.Millisecond, // Initial delay
		2*time.Second,        // Max delay
		2.0,                  // Multiplier
	)),
	romancy.WithTimeout[string, int](10*time.Second),
)

// BadRequestAPICall simulates an API that returns non-retryable errors.
var BadRequestAPICall = romancy.DefineActivity(
	"bad_request_api_call",
	func(ctx context.Context, endpoint string) (int, error) {
		attempt := atomic.AddInt64(&failingCallAttempts, 1)
		log.Printf("[Activity] Calling API with bad request: %s (attempt %d)", endpoint, attempt)

		// This error should not be retried
		return 0, fmt.Errorf("invalid parameters: %w", ErrBadRequest)
	},
	// Policy with non-retryable errors
	romancy.WithRetryPolicy[string, int](
		retry.NewBuilder().
			WithMaxAttempts(5).
			WithInitialInterval(100*time.Millisecond).
			WithNonRetryableErrors(ErrBadRequest, ErrNotFound).
			Build(),
	),
)

// RateLimitedAPICall simulates an API with rate limiting.
var RateLimitedAPICall = romancy.DefineActivity(
	"rate_limited_api_call",
	func(ctx context.Context, endpoint string) (int, error) {
		log.Printf("[Activity] Calling rate-limited API: %s", endpoint)

		// Simulate rate limiting for first 2 calls
		if rand.Float64() < 0.6 {
			log.Printf("[Activity] Rate limited (429)")
			return 429, ErrRateLimited
		}

		log.Printf("[Activity] Success (200)")
		return 200, nil
	},
	// Fixed interval retry for rate limiting (1 second between retries)
	romancy.WithRetryPolicy[string, int](retry.Fixed(5, 1*time.Second)),
)

// ----- Workflows -----

// flakyAPIWorkflow calls a flaky API with automatic retries.
var flakyAPIWorkflow = romancy.DefineWorkflow("flaky_api_workflow",
	func(ctx *romancy.WorkflowContext, input APICallInput) (APICallResult, error) {
		log.Printf("[Workflow] Starting flaky API workflow for: %s", input.Endpoint)

		result := APICallResult{
			Endpoint: input.Endpoint,
			Status:   "processing",
		}

		// Reset counter for this workflow
		atomic.StoreInt64(&flakyCallAttempts, 0)

		// Call the flaky API (will retry automatically)
		responseCode, err := FlakyAPICall.Execute(ctx, input.Endpoint)
		if err != nil {
			result.Status = "failed"
			result.Attempts = int(atomic.LoadInt64(&flakyCallAttempts))
			return result, err
		}

		result.ResponseCode = responseCode
		result.Attempts = int(atomic.LoadInt64(&flakyCallAttempts))
		result.Status = "success"

		log.Printf("[Workflow] Completed after %d attempts", result.Attempts)
		return result, nil
	},
)

// nonRetryableWorkflow demonstrates non-retryable errors.
var nonRetryableWorkflow = romancy.DefineWorkflow("non_retryable_workflow",
	func(ctx *romancy.WorkflowContext, input APICallInput) (APICallResult, error) {
		log.Printf("[Workflow] Starting non-retryable workflow for: %s", input.Endpoint)

		result := APICallResult{
			Endpoint: input.Endpoint,
			Status:   "processing",
		}

		// Reset counter for this workflow
		atomic.StoreInt64(&failingCallAttempts, 0)

		// Call the API with bad request (should NOT retry)
		responseCode, err := BadRequestAPICall.Execute(ctx, input.Endpoint)
		if err != nil {
			result.Status = "failed_immediately"
			result.Attempts = int(atomic.LoadInt64(&failingCallAttempts))
			log.Printf("[Workflow] Failed immediately without retries (as expected)")
			return result, err
		}

		result.ResponseCode = responseCode
		result.Attempts = int(atomic.LoadInt64(&failingCallAttempts))
		result.Status = "success"
		return result, nil
	},
)

// rateLimitedWorkflow handles rate-limited APIs.
var rateLimitedWorkflow = romancy.DefineWorkflow("rate_limited_workflow",
	func(ctx *romancy.WorkflowContext, input APICallInput) (APICallResult, error) {
		log.Printf("[Workflow] Starting rate-limited workflow for: %s", input.Endpoint)

		result := APICallResult{
			Endpoint: input.Endpoint,
			Status:   "processing",
		}

		// Call the rate-limited API (will retry with fixed interval)
		responseCode, err := RateLimitedAPICall.Execute(ctx, input.Endpoint)
		if err != nil {
			result.Status = "failed"
			return result, err
		}

		result.ResponseCode = responseCode
		result.Status = "success"

		log.Printf("[Workflow] Completed successfully")
		return result, nil
	},
)

// ----- OpenTelemetry Setup -----

func setupTracing(ctx context.Context) (*sdktrace.TracerProvider, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil // No tracing configured
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "romancy-retry"
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(10*time.Second),
		otlptracegrpc.WithRetry(otlptracegrpc.RetryConfig{Enabled: true}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
	)
	return tp, nil
}

// ----- Main -----

func main() {
	ctx := context.Background()

	// Setup OpenTelemetry tracing
	tp, err := setupTracing(ctx)
	if err != nil {
		log.Printf("Warning: Failed to setup tracing: %v", err)
	}
	if tp != nil {
		defer func() {
			if err := tp.Shutdown(ctx); err != nil {
				log.Printf("Error shutting down tracer: %v", err)
			}
		}()
	}

	// Get database URL from environment or use default
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "retry_demo.db"
	}

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbURL),
		romancy.WithWorkerID("retry-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Register workflows
	romancy.RegisterWorkflow[APICallInput, APICallResult](app, flakyAPIWorkflow)
	romancy.RegisterWorkflow[APICallInput, APICallResult](app, nonRetryableWorkflow)
	romancy.RegisterWorkflow[APICallInput, APICallResult](app, rateLimitedWorkflow)

	// Start the application
	if err := app.Start(ctx); err != nil {
		log.Printf("Failed to start app: %v", err)
		return
	}
	defer func() {
		if err := app.Shutdown(ctx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	log.Println("==============================================")
	log.Println("Retry Policy Example")
	log.Println("==============================================")

	// Example 1: Flaky API with exponential backoff
	log.Println("\n--- Example 1: Flaky API (Exponential Backoff) ---")
	flakyInput := APICallInput{
		Endpoint:   "/api/v1/flaky",
		MaxRetries: 5,
	}
	flakyID, _ := romancy.StartWorkflow(ctx, app, flakyAPIWorkflow, flakyInput)
	log.Printf("Started flaky API workflow: %s", flakyID)

	// Wait for completion
	time.Sleep(2 * time.Second)

	result1, err := romancy.GetWorkflowResult[APICallResult](ctx, app, flakyID)
	if err != nil {
		log.Printf("Workflow error: %v", err)
	} else {
		log.Printf("Result: %+v", result1.Output)
	}

	// Example 2: Non-retryable error
	log.Println("\n--- Example 2: Non-Retryable Error (Bad Request) ---")
	badRequestInput := APICallInput{
		Endpoint:   "/api/v1/bad-request",
		MaxRetries: 5,
	}
	badRequestID, _ := romancy.StartWorkflow(ctx, app, nonRetryableWorkflow, badRequestInput)
	log.Printf("Started non-retryable workflow: %s", badRequestID)

	// Wait for completion
	time.Sleep(500 * time.Millisecond)

	result2, err := romancy.GetWorkflowResult[APICallResult](ctx, app, badRequestID)
	if err != nil {
		log.Printf("Workflow error (expected): %v", err)
	}
	log.Printf("Result: Attempts=%d (should be 1, no retries)", result2.Output.Attempts)

	// Example 3: Rate-limited API with fixed interval
	log.Println("\n--- Example 3: Rate-Limited API (Fixed Interval) ---")
	rateLimitInput := APICallInput{
		Endpoint:   "/api/v1/rate-limited",
		MaxRetries: 5,
	}
	rateLimitID, _ := romancy.StartWorkflow(ctx, app, rateLimitedWorkflow, rateLimitInput)
	log.Printf("Started rate-limited workflow: %s", rateLimitID)

	// Wait for completion
	time.Sleep(5 * time.Second)

	result3, err := romancy.GetWorkflowResult[APICallResult](ctx, app, rateLimitID)
	if err != nil {
		log.Printf("Workflow error: %v", err)
	} else {
		log.Printf("Result: %+v", result3.Output)
	}

	log.Println("\n==============================================")
	log.Println("Retry Policy Examples Completed!")
	log.Println("==============================================")

	// In serve mode (for Tilt), wait for shutdown signal instead of exiting
	if os.Getenv("ROMANCY_SERVE") == "true" {
		log.Println("\nRunning in serve mode. Press Ctrl+C to stop.")
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("\nShutting down...")
	}
}
