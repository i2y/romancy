package compensation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRegistry(t *testing.T) {
	r := NewRegistry()

	// Register a function
	called := false
	r.Register("test_func", func(ctx context.Context, arg []byte) error {
		called = true
		return nil
	})

	// Get the function
	fn, ok := r.Get("test_func")
	if !ok {
		t.Error("expected to find registered function")
	}

	// Execute the function
	err := fn(context.Background(), nil)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected function to be called")
	}

	// Get non-existent function
	_, ok = r.Get("nonexistent")
	if ok {
		t.Error("expected to not find non-existent function")
	}
}

func TestTypedCompensation(t *testing.T) {
	type TestArg struct {
		OrderID string `json:"order_id"`
		Amount  int    `json:"amount"`
	}

	r := NewRegistry()

	var receivedArg TestArg
	tc := NewTypedCompensation("cancel_order", func(ctx context.Context, arg TestArg) error {
		receivedArg = arg
		return nil
	})

	tc.Register(r)

	// Get and execute
	fn, ok := r.Get("cancel_order")
	if !ok {
		t.Error("expected to find registered function")
	}

	arg := TestArg{OrderID: "ORD-123", Amount: 100}
	argBytes, _ := json.Marshal(arg)

	err := fn(context.Background(), argBytes)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	if receivedArg.OrderID != "ORD-123" {
		t.Errorf("expected OrderID=ORD-123, got %s", receivedArg.OrderID)
	}
	if receivedArg.Amount != 100 {
		t.Errorf("expected Amount=100, got %d", receivedArg.Amount)
	}
}

func TestTypedCompensation_InvalidJSON(t *testing.T) {
	type TestArg struct {
		Value int `json:"value"`
	}

	r := NewRegistry()

	tc := NewTypedCompensation("test_func", func(ctx context.Context, arg TestArg) error {
		return nil
	})

	tc.Register(r)

	fn, _ := r.Get("test_func")

	// Pass invalid JSON
	err := fn(context.Background(), []byte("not valid json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestTypedCompensation_Error(t *testing.T) {
	type TestArg struct{}

	expectedErr := errors.New("compensation failed")

	tc := NewTypedCompensation("test_func", func(ctx context.Context, arg TestArg) error {
		return expectedErr
	})

	fn := tc.AsFunc()
	err := fn(context.Background(), []byte("{}"))

	if !errors.Is(err, expectedErr) {
		t.Errorf("expected error %v, got %v", expectedErr, err)
	}
}

func TestCompensationError(t *testing.T) {
	origErr := errors.New("original error")
	compErr := &CompensationError{
		ActivityID: "reserve_inventory:1",
		FuncName:   "cancel_reservation",
		Err:        origErr,
	}

	// Test Error()
	errStr := compErr.Error()
	if errStr != "compensation failed for activity reserve_inventory:1 (cancel_reservation): original error" {
		t.Errorf("unexpected error string: %s", errStr)
	}

	// Test Unwrap()
	if !errors.Is(compErr, origErr) {
		t.Error("expected Unwrap to return original error")
	}
}
