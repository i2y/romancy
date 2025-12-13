package storage

import (
	"testing"
)

func TestValidateJSONPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		// Valid paths
		{name: "simple field", path: "name", wantErr: false},
		{name: "underscore field", path: "user_id", wantErr: false},
		{name: "nested path", path: "order.customer.id", wantErr: false},
		{name: "with numbers", path: "item1", wantErr: false},
		{name: "complex nested", path: "data.customer.address.zip_code", wantErr: false},
		{name: "starts with underscore", path: "_internal", wantErr: false},

		// Invalid paths
		{name: "empty", path: "", wantErr: true},
		{name: "starts with number", path: "1field", wantErr: true},
		{name: "contains space", path: "order id", wantErr: true},
		{name: "SQL injection attempt 1", path: "'; DROP TABLE workflow_instances; --", wantErr: true},
		{name: "SQL injection attempt 2", path: "id OR 1=1", wantErr: true},
		{name: "contains semicolon", path: "field;", wantErr: true},
		{name: "contains quotes", path: "field'", wantErr: true},
		{name: "contains brackets", path: "field[0]", wantErr: true},
		{name: "contains parentheses", path: "field()", wantErr: true},
		{name: "starts with dot", path: ".field", wantErr: true},
		{name: "ends with dot", path: "field.", wantErr: true},
		{name: "consecutive dots", path: "field..name", wantErr: true},
		{name: "contains hyphen", path: "field-name", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateJSONPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateJSONPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestSQLiteDriver_JSONExtract(t *testing.T) {
	driver := &SQLiteDriver{}

	tests := []struct {
		column   string
		path     string
		expected string
	}{
		{"input_data", "name", "json_extract(input_data, '$.name')"},
		{"input_data", "order.customer.id", "json_extract(input_data, '$.order.customer.id')"},
	}

	for _, tt := range tests {
		result := driver.JSONExtract(tt.column, tt.path)
		if result != tt.expected {
			t.Errorf("JSONExtract(%q, %q) = %q, want %q", tt.column, tt.path, result, tt.expected)
		}
	}
}

func TestPostgresDriver_JSONExtract(t *testing.T) {
	driver := &PostgresDriver{}

	tests := []struct {
		column   string
		path     string
		expected string
	}{
		{"input_data", "name", "(input_data::jsonb #>> '{name}')"},
		{"input_data", "order.customer.id", "(input_data::jsonb #>> '{order,customer,id}')"},
	}

	for _, tt := range tests {
		result := driver.JSONExtract(tt.column, tt.path)
		if result != tt.expected {
			t.Errorf("JSONExtract(%q, %q) = %q, want %q", tt.column, tt.path, result, tt.expected)
		}
	}
}

func TestMySQLDriver_JSONExtract(t *testing.T) {
	driver := &MySQLDriver{}

	tests := []struct {
		column   string
		path     string
		expected string
	}{
		{"input_data", "name", "JSON_UNQUOTE(JSON_EXTRACT(input_data, '$.name'))"},
		{"input_data", "order.customer.id", "JSON_UNQUOTE(JSON_EXTRACT(input_data, '$.order.customer.id'))"},
	}

	for _, tt := range tests {
		result := driver.JSONExtract(tt.column, tt.path)
		if result != tt.expected {
			t.Errorf("JSONExtract(%q, %q) = %q, want %q", tt.column, tt.path, result, tt.expected)
		}
	}
}

func TestSQLiteDriver_JSONCompare(t *testing.T) {
	driver := &SQLiteDriver{}
	extractExpr := "json_extract(input_data, '$.name')"

	tests := []struct {
		name        string
		value       any
		wantExpr    string
		wantArgsLen int
		wantArgsVal any
	}{
		{
			name:        "string value",
			value:       "test",
			wantExpr:    "json_extract(input_data, '$.name') = ?",
			wantArgsLen: 1,
			wantArgsVal: "test",
		},
		{
			name:        "int value",
			value:       42,
			wantExpr:    "CAST(json_extract(input_data, '$.name') AS REAL) = ?",
			wantArgsLen: 1,
			wantArgsVal: float64(42),
		},
		{
			name:        "float value",
			value:       3.14,
			wantExpr:    "CAST(json_extract(input_data, '$.name') AS REAL) = ?",
			wantArgsLen: 1,
			wantArgsVal: 3.14,
		},
		{
			name:        "bool true",
			value:       true,
			wantExpr:    "(json_extract(input_data, '$.name') = 1 OR json_extract(input_data, '$.name') = 'true')",
			wantArgsLen: 0,
		},
		{
			name:        "bool false",
			value:       false,
			wantExpr:    "(json_extract(input_data, '$.name') = 0 OR json_extract(input_data, '$.name') = 'false')",
			wantArgsLen: 0,
		},
		{
			name:        "nil value",
			value:       nil,
			wantExpr:    "(json_extract(input_data, '$.name') IS NULL OR json_extract(input_data, '$.name') = 'null')",
			wantArgsLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, args := driver.JSONCompare(extractExpr, tt.value, 1)
			if expr != tt.wantExpr {
				t.Errorf("JSONCompare expr = %q, want %q", expr, tt.wantExpr)
			}
			if len(args) != tt.wantArgsLen {
				t.Errorf("JSONCompare args len = %d, want %d", len(args), tt.wantArgsLen)
			}
			if tt.wantArgsLen > 0 && args[0] != tt.wantArgsVal {
				t.Errorf("JSONCompare args[0] = %v, want %v", args[0], tt.wantArgsVal)
			}
		})
	}
}

func TestInputFilterBuilder_BuildFilterQuery(t *testing.T) {
	driver := &SQLiteDriver{}
	builder := NewInputFilterBuilder(driver)

	t.Run("empty filters", func(t *testing.T) {
		conditions, args, err := builder.BuildFilterQuery(nil, 1)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if len(conditions) != 0 {
			t.Errorf("expected 0 conditions, got %d", len(conditions))
		}
		if len(args) != 0 {
			t.Errorf("expected 0 args, got %d", len(args))
		}
	})

	t.Run("single string filter", func(t *testing.T) {
		filters := map[string]any{"name": "test"}
		conditions, args, err := builder.BuildFilterQuery(filters, 1)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if len(conditions) != 1 {
			t.Errorf("expected 1 condition, got %d", len(conditions))
		}
		if len(args) != 1 {
			t.Errorf("expected 1 arg, got %d", len(args))
		}
		if args[0] != "test" {
			t.Errorf("expected arg 'test', got %v", args[0])
		}
	})

	t.Run("multiple filters", func(t *testing.T) {
		filters := map[string]any{
			"name":    "test",
			"user_id": 123,
		}
		conditions, args, err := builder.BuildFilterQuery(filters, 1)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if len(conditions) != 2 {
			t.Errorf("expected 2 conditions, got %d", len(conditions))
		}
		if len(args) != 2 {
			t.Errorf("expected 2 args, got %d", len(args))
		}
	})

	t.Run("invalid path", func(t *testing.T) {
		filters := map[string]any{"'; DROP TABLE users; --": "test"}
		_, _, err := builder.BuildFilterQuery(filters, 1)
		if err == nil {
			t.Error("expected error for invalid path")
		}
	})

	t.Run("nested path", func(t *testing.T) {
		filters := map[string]any{"order.customer.id": "cust_123"}
		conditions, args, err := builder.BuildFilterQuery(filters, 1)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if len(conditions) != 1 {
			t.Errorf("expected 1 condition, got %d", len(conditions))
		}
		if len(args) != 1 {
			t.Errorf("expected 1 arg, got %d", len(args))
		}
	})
}
