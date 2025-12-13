// Package main demonstrates the Saga pattern with compensation.
//
// This example shows:
// - Multiple activities with compensation functions
// - Automatic compensation on failure (LIFO order)
// - Transaction-like behavior without distributed transactions
//
// The workflow simulates a trip booking:
// 1. Book flight
// 2. Reserve hotel
// 3. Rent car
// 4. Process payment (fails to trigger compensations)
//
// Usage:
//
//	go run ./examples/saga/
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/i2y/romancy"
	"github.com/i2y/romancy/hooks/otel"
)

// ----- Types -----

// TripBookingInput is the input for the trip booking workflow.
type TripBookingInput struct {
	UserID      string `json:"user_id"`
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	StartDate   string `json:"start_date"`
	EndDate     string `json:"end_date"`
	ShouldFail  bool   `json:"should_fail"` // For testing compensation
}

// TripBookingResult is the output of the trip booking workflow.
type TripBookingResult struct {
	UserID            string `json:"user_id"`
	FlightBookingID   string `json:"flight_booking_id,omitempty"`
	HotelReservation  string `json:"hotel_reservation,omitempty"`
	CarRentalID       string `json:"car_rental_id,omitempty"`
	PaymentID         string `json:"payment_id,omitempty"`
	Status            string `json:"status"`
	CompensationNotes string `json:"compensation_notes,omitempty"`
}

// BookingDetails contains details for a booking step.
type BookingDetails struct {
	UserID      string
	Origin      string
	Destination string
	StartDate   string
	EndDate     string
}

// ----- Errors -----

// ErrPaymentFailed indicates a payment failure.
var ErrPaymentFailed = errors.New("payment processing failed")

// ----- Activities -----

// BookFlight books a flight and provides compensation to cancel it.
var BookFlight = romancy.DefineActivity(
	"book_flight",
	func(ctx context.Context, details BookingDetails) (string, error) {
		log.Printf("[Activity] Booking flight: %s -> %s", details.Origin, details.Destination)
		time.Sleep(100 * time.Millisecond) // Simulate API call

		bookingID := fmt.Sprintf("FL-%s-%d", details.UserID, time.Now().UnixNano()%10000)
		log.Printf("[Activity] Flight booked: %s", bookingID)
		return bookingID, nil
	},
	romancy.WithCompensation[BookingDetails, string](func(ctx context.Context, details BookingDetails) error {
		log.Printf("[Compensation] Canceling flight booking for user %s", details.UserID)
		log.Printf("[Compensation] Refunding flight from %s to %s", details.Origin, details.Destination)
		time.Sleep(50 * time.Millisecond) // Simulate cancellation
		log.Printf("[Compensation] Flight booking canceled successfully")
		return nil
	}),
)

// ReserveHotel reserves a hotel and provides compensation to cancel it.
var ReserveHotel = romancy.DefineActivity(
	"reserve_hotel",
	func(ctx context.Context, details BookingDetails) (string, error) {
		log.Printf("[Activity] Reserving hotel in %s from %s to %s",
			details.Destination, details.StartDate, details.EndDate)
		time.Sleep(100 * time.Millisecond) // Simulate API call

		reservationID := fmt.Sprintf("HTL-%s-%d", details.UserID, time.Now().UnixNano()%10000)
		log.Printf("[Activity] Hotel reserved: %s", reservationID)
		return reservationID, nil
	},
	romancy.WithCompensation[BookingDetails, string](func(ctx context.Context, details BookingDetails) error {
		log.Printf("[Compensation] Canceling hotel reservation for user %s", details.UserID)
		log.Printf("[Compensation] Releasing room in %s", details.Destination)
		time.Sleep(50 * time.Millisecond) // Simulate cancellation
		log.Printf("[Compensation] Hotel reservation canceled successfully")
		return nil
	}),
)

// RentCar rents a car and provides compensation to cancel it.
var RentCar = romancy.DefineActivity(
	"rent_car",
	func(ctx context.Context, details BookingDetails) (string, error) {
		log.Printf("[Activity] Renting car in %s from %s to %s",
			details.Destination, details.StartDate, details.EndDate)
		time.Sleep(100 * time.Millisecond) // Simulate API call

		rentalID := fmt.Sprintf("CAR-%s-%d", details.UserID, time.Now().UnixNano()%10000)
		log.Printf("[Activity] Car rented: %s", rentalID)
		return rentalID, nil
	},
	romancy.WithCompensation[BookingDetails, string](func(ctx context.Context, details BookingDetails) error {
		log.Printf("[Compensation] Canceling car rental for user %s", details.UserID)
		log.Printf("[Compensation] Releasing car reservation in %s", details.Destination)
		time.Sleep(50 * time.Millisecond) // Simulate cancellation
		log.Printf("[Compensation] Car rental canceled successfully")
		return nil
	}),
)

// ProcessPayment processes the payment (may fail to demonstrate compensation).
var ProcessPayment = romancy.DefineActivity(
	"process_payment",
	func(ctx context.Context, input struct {
		UserID     string
		ShouldFail bool
	}) (string, error) {
		log.Printf("[Activity] Processing payment for user %s", input.UserID)
		time.Sleep(100 * time.Millisecond) // Simulate processing

		if input.ShouldFail {
			log.Printf("[Activity] Payment FAILED! (simulated failure)")
			return "", fmt.Errorf("card declined: %w", ErrPaymentFailed)
		}

		paymentID := fmt.Sprintf("PAY-%s-%d", input.UserID, time.Now().UnixNano()%10000)
		log.Printf("[Activity] Payment processed: %s", paymentID)
		return paymentID, nil
	},
)

// ----- Workflow -----

// tripBookingWorkflow orchestrates the entire trip booking process.
var tripBookingWorkflow = romancy.DefineWorkflow("trip_booking_workflow",
	func(ctx *romancy.WorkflowContext, input TripBookingInput) (TripBookingResult, error) {
		log.Printf("[Workflow] Starting trip booking for user %s", input.UserID)
		log.Printf("[Workflow] Trip: %s -> %s (%s to %s)",
			input.Origin, input.Destination, input.StartDate, input.EndDate)

		result := TripBookingResult{
			UserID: input.UserID,
			Status: "processing",
		}

		details := BookingDetails{
			UserID:      input.UserID,
			Origin:      input.Origin,
			Destination: input.Destination,
			StartDate:   input.StartDate,
			EndDate:     input.EndDate,
		}

		// Step 1: Book flight (with compensation registered)
		log.Printf("\n[Workflow] Step 1: Booking flight...")
		flightID, err := BookFlight.Execute(ctx, details)
		if err != nil {
			result.Status = "flight_booking_failed"
			return result, fmt.Errorf("failed to book flight: %w", err)
		}
		result.FlightBookingID = flightID
		log.Printf("[Workflow] Flight booked: %s", flightID)

		// Step 2: Reserve hotel (with compensation registered)
		log.Printf("\n[Workflow] Step 2: Reserving hotel...")
		hotelID, err := ReserveHotel.Execute(ctx, details)
		if err != nil {
			// Flight compensation will be executed automatically
			result.Status = "hotel_reservation_failed"
			return result, fmt.Errorf("failed to reserve hotel: %w", err)
		}
		result.HotelReservation = hotelID
		log.Printf("[Workflow] Hotel reserved: %s", hotelID)

		// Step 3: Rent car (with compensation registered)
		log.Printf("\n[Workflow] Step 3: Renting car...")
		carID, err := RentCar.Execute(ctx, details)
		if err != nil {
			// Hotel and flight compensations will be executed automatically
			result.Status = "car_rental_failed"
			return result, fmt.Errorf("failed to rent car: %w", err)
		}
		result.CarRentalID = carID
		log.Printf("[Workflow] Car rented: %s", carID)

		// Step 4: Process payment (may fail)
		log.Printf("\n[Workflow] Step 4: Processing payment...")
		paymentID, err := ProcessPayment.Execute(ctx, struct {
			UserID     string
			ShouldFail bool
		}{
			UserID:     input.UserID,
			ShouldFail: input.ShouldFail,
		})
		if err != nil {
			// All compensations (car, hotel, flight) will be executed in LIFO order!
			result.Status = "payment_failed"
			result.CompensationNotes = "All bookings will be automatically canceled"
			log.Printf("[Workflow] Payment failed! Compensations will be executed...")
			return result, fmt.Errorf("payment failed: %w", err)
		}
		result.PaymentID = paymentID
		log.Printf("[Workflow] Payment processed: %s", paymentID)

		result.Status = "completed"
		log.Printf("\n[Workflow] Trip booking completed successfully!")
		log.Printf("[Workflow] Summary:")
		log.Printf("  - Flight: %s", result.FlightBookingID)
		log.Printf("  - Hotel: %s", result.HotelReservation)
		log.Printf("  - Car: %s", result.CarRentalID)
		log.Printf("  - Payment: %s", result.PaymentID)

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
		serviceName = "romancy-saga"
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
		dbURL = "saga_demo.db"
	}

	// Create the application with optional tracing
	opts := []romancy.Option{
		romancy.WithDatabase(dbURL),
		romancy.WithWorkerID("saga-worker-1"),
	}
	if tp != nil {
		opts = append(opts, romancy.WithHooks(otel.NewOTelHooks(tp)))
	}
	app := romancy.NewApp(opts...)

	// Register the workflow
	romancy.RegisterWorkflow[TripBookingInput, TripBookingResult](app, tripBookingWorkflow)

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
	log.Println("Saga Pattern Example (Trip Booking)")
	log.Println("==============================================")

	// Example 1: Successful booking
	log.Println("\n========== EXAMPLE 1: Successful Booking ==========")
	successInput := TripBookingInput{
		UserID:      "user-001",
		Origin:      "New York",
		Destination: "Paris",
		StartDate:   "2024-06-01",
		EndDate:     "2024-06-10",
		ShouldFail:  false,
	}

	successID, _ := romancy.StartWorkflow(ctx, app, tripBookingWorkflow, successInput)
	log.Printf("Started successful booking workflow: %s", successID)

	time.Sleep(1 * time.Second)

	result1, err := romancy.GetWorkflowResult[TripBookingResult](ctx, app, successID)
	if err != nil {
		log.Printf("Workflow error: %v", err)
	} else {
		log.Printf("\nFinal Result: %s", result1.Status)
	}

	// Example 2: Failed booking (triggers compensation)
	log.Println("\n========== EXAMPLE 2: Failed Booking (Compensation Demo) ==========")
	log.Println("This will book flight, hotel, and car, then FAIL payment.")
	log.Println("Watch for compensations being executed in LIFO order (car -> hotel -> flight)")
	log.Println()

	failInput := TripBookingInput{
		UserID:      "user-002",
		Origin:      "Tokyo",
		Destination: "London",
		StartDate:   "2024-07-01",
		EndDate:     "2024-07-15",
		ShouldFail:  true, // This will trigger compensation!
	}

	failID, _ := romancy.StartWorkflow(ctx, app, tripBookingWorkflow, failInput)
	log.Printf("Started failing booking workflow: %s", failID)

	time.Sleep(2 * time.Second)

	result2, err := romancy.GetWorkflowResult[TripBookingResult](ctx, app, failID)
	if err != nil {
		log.Printf("Workflow error (expected): %v", err)
	}
	log.Printf("\nFinal Result: %s", result2.Output.Status)
	if result2.Output.CompensationNotes != "" {
		log.Printf("Compensation Notes: %s", result2.Output.CompensationNotes)
	}

	log.Println("\n==============================================")
	log.Println("Saga Pattern Examples Completed!")
	log.Println("")
	log.Println("Key Points:")
	log.Println("- Compensations are registered when activities succeed")
	log.Println("- If a later step fails, compensations run in LIFO order")
	log.Println("- This provides transaction-like behavior without 2PC")
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
