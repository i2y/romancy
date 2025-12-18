package romancy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandler(t *testing.T) {
	app := NewApp(WithDatabase(":memory:"))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()

	handler := app.Handler()
	if handler == nil {
		t.Fatal("Handler() returned nil")
	}

	t.Run("CloudEvents endpoint accepts POST", func(t *testing.T) {
		event := `{"type":"test.event","source":"test","specversion":"1.0","id":"123"}`
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(event))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Errorf("Expected status %d, got %d", http.StatusAccepted, w.Code)
		}
	})

	t.Run("CloudEvents endpoint rejects GET", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("Expected status %d, got %d", http.StatusMethodNotAllowed, w.Code)
		}
	})

	t.Run("Health live endpoint", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
		}
	})

	t.Run("Health ready endpoint", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
		}
	})

	t.Run("Invalid JSON returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("invalid json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
		}

		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if resp["retryable"] != "false" {
			t.Errorf("Expected retryable=false, got %s", resp["retryable"])
		}
	})

	t.Run("Missing type field returns 400", func(t *testing.T) {
		event := `{"source":"test","specversion":"1.0","id":"123"}`
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(event))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("Expected status %d, got %d", http.StatusBadRequest, w.Code)
		}

		var resp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if resp["error_type"] != "ValidationError" {
			t.Errorf("Expected error_type=ValidationError, got %s", resp["error_type"])
		}
	})
}

func TestHandlerIntegrationWithServeMux(t *testing.T) {
	app := NewApp(WithDatabase(":memory:"))
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("Failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(context.Background()) }()

	// Simulate integration with standard http.ServeMux
	mainMux := http.NewServeMux()
	mainMux.Handle("/workflows/", http.StripPrefix("/workflows", app.Handler()))

	t.Run("Mounted handler works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/workflows/health/live", nil)
		w := httptest.NewRecorder()

		mainMux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status %d, got %d", http.StatusOK, w.Code)
		}
	})

	t.Run("CloudEvents via mounted handler", func(t *testing.T) {
		event := `{"type":"test.event","source":"test","specversion":"1.0","id":"456"}`
		req := httptest.NewRequest(http.MethodPost, "/workflows/", strings.NewReader(event))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		mainMux.ServeHTTP(w, req)

		if w.Code != http.StatusAccepted {
			t.Errorf("Expected status %d, got %d", http.StatusAccepted, w.Code)
		}
	})
}

func TestCalculateBackoff(t *testing.T) {
	base := 1 * time.Second
	maxBackoff := 30 * time.Second

	tests := []struct {
		name             string
		consecutiveEmpty int
		expected         time.Duration
	}{
		{"zero consecutive empty returns base", 0, 1 * time.Second},
		{"1 consecutive empty returns 2x base", 1, 2 * time.Second},
		{"2 consecutive empty returns 4x base", 2, 4 * time.Second},
		{"3 consecutive empty returns 8x base", 3, 8 * time.Second},
		{"4 consecutive empty returns 16x base", 4, 16 * time.Second},
		{"5 consecutive empty returns 32x base (capped)", 5, 30 * time.Second},
		{"10 consecutive empty returns max (exponent capped at 5)", 10, 30 * time.Second},
		{"100 consecutive empty returns max", 100, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateBackoff(base, tt.consecutiveEmpty, maxBackoff)
			if result != tt.expected {
				t.Errorf("calculateBackoff(%v, %d, %v) = %v, want %v",
					base, tt.consecutiveEmpty, maxBackoff, result, tt.expected)
			}
		})
	}
}

func TestCalculateBackoff_DifferentBase(t *testing.T) {
	// Test with different base intervals
	tests := []struct {
		name             string
		base             time.Duration
		consecutiveEmpty int
		maxBackoff       time.Duration
		expected         time.Duration
	}{
		{"100ms base, 2 empty", 100 * time.Millisecond, 2, 1 * time.Second, 400 * time.Millisecond},
		{"500ms base, 3 empty", 500 * time.Millisecond, 3, 10 * time.Second, 4 * time.Second},
		{"2s base, 1 empty", 2 * time.Second, 1, 60 * time.Second, 4 * time.Second},
		{"1s base, 5 empty, low max", 1 * time.Second, 5, 10 * time.Second, 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateBackoff(tt.base, tt.consecutiveEmpty, tt.maxBackoff)
			if result != tt.expected {
				t.Errorf("calculateBackoff(%v, %d, %v) = %v, want %v",
					tt.base, tt.consecutiveEmpty, tt.maxBackoff, result, tt.expected)
			}
		})
	}
}

func TestAddJitter(t *testing.T) {
	base := 1 * time.Second

	// Run multiple times to verify jitter is within range
	for i := 0; i < 100; i++ {
		result := addJitter(base)
		// addJitter adds ±25% jitter, so result should be in [0.75*base, 1.25*base]
		minExpected := time.Duration(float64(base) * 0.75)
		maxExpected := time.Duration(float64(base) * 1.25)

		if result < minExpected || result > maxExpected {
			t.Errorf("addJitter(%v) = %v, expected in range [%v, %v]",
				base, result, minExpected, maxExpected)
		}
	}
}

func TestAddJitterFraction(t *testing.T) {
	tests := []struct {
		name           string
		base           time.Duration
		jitterFraction float64
	}{
		{"10% jitter", 1 * time.Second, 0.1},
		{"30% jitter", 1 * time.Second, 0.3},
		{"50% jitter", 1 * time.Second, 0.5},
		{"0% jitter (no jitter)", 1 * time.Second, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 100; i++ {
				result := addJitterFraction(tt.base, tt.jitterFraction)
				// addJitterFraction adds [0, jitterFraction*base] jitter
				minExpected := tt.base
				maxExpected := time.Duration(float64(tt.base) * (1.0 + tt.jitterFraction))

				if result < minExpected || result > maxExpected {
					t.Errorf("addJitterFraction(%v, %v) = %v, expected in range [%v, %v]",
						tt.base, tt.jitterFraction, result, minExpected, maxExpected)
				}
			}
		})
	}
}
