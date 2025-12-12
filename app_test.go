package romancy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
