package storage

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// validJSONPathPattern validates JSON paths to prevent SQL injection.
// Allows: alphanumeric characters, underscores, and dots for nested paths.
// Does not allow: starting/ending with dot, consecutive dots.
// Examples: "name", "order.id", "customer.address.city"
var validJSONPathPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)*$`)

// ValidateJSONPath validates a JSON path for safe use in SQL queries.
// Returns an error if the path contains potentially dangerous characters.
func ValidateJSONPath(path string) error {
	if path == "" {
		return fmt.Errorf("empty JSON path")
	}
	if !validJSONPathPattern.MatchString(path) {
		return fmt.Errorf("invalid JSON path: %q (only alphanumeric, underscore, and dot allowed)", path)
	}
	return nil
}

// InputFilterBuilder builds SQL query conditions for input filters.
type InputFilterBuilder struct {
	driver Driver
}

// NewInputFilterBuilder creates a new InputFilterBuilder.
func NewInputFilterBuilder(driver Driver) *InputFilterBuilder {
	return &InputFilterBuilder{driver: driver}
}

// BuildFilterQuery builds the WHERE clause conditions for input filters.
// Returns the query parts (as a slice of conditions) and args to append,
// or an error if validation fails.
// The startPlaceholder is the starting placeholder number (for PostgreSQL).
func (b *InputFilterBuilder) BuildFilterQuery(inputFilters map[string]any, startPlaceholder int) (conditions []string, args []any, err error) {
	if len(inputFilters) == 0 {
		return nil, nil, nil
	}

	conditions = make([]string, 0, len(inputFilters))
	placeholderNum := startPlaceholder

	// Sort keys for deterministic query generation
	keys := make([]string, 0, len(inputFilters))
	for k := range inputFilters {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, path := range keys {
		value := inputFilters[path]

		// Validate the path to prevent SQL injection
		if err := ValidateJSONPath(path); err != nil {
			return nil, nil, err
		}

		// Generate the JSON extraction expression
		extractExpr := b.driver.JSONExtract("input_data", path)

		// Generate the comparison expression
		compExpr, compArgs := b.driver.JSONCompare(extractExpr, value, placeholderNum)
		conditions = append(conditions, compExpr)
		args = append(args, compArgs...)
		placeholderNum += len(compArgs)
	}

	return conditions, args, nil
}

// JSONExtract for SQLiteDriver returns the SQL expression to extract a value from a JSON column.
func (d *SQLiteDriver) JSONExtract(column, path string) string {
	// SQLite uses json_extract with $ notation
	// For nested paths like "order.customer.id", becomes "$.order.customer.id"
	return fmt.Sprintf("json_extract(%s, '$.%s')", column, path)
}

// JSONCompare for SQLiteDriver returns the SQL expression to compare a JSON-extracted value.
func (d *SQLiteDriver) JSONCompare(extractExpr string, value any, _ int) (expr string, compArgs []any) {
	switch v := value.(type) {
	case nil:
		return fmt.Sprintf("(%s IS NULL OR %s = 'null')", extractExpr, extractExpr), nil
	case bool:
		if v {
			return fmt.Sprintf("(%s = 1 OR %s = 'true')", extractExpr, extractExpr), nil
		}
		return fmt.Sprintf("(%s = 0 OR %s = 'false')", extractExpr, extractExpr), nil
	case float64:
		return fmt.Sprintf("CAST(%s AS REAL) = ?", extractExpr), []any{v}
	case int:
		return fmt.Sprintf("CAST(%s AS REAL) = ?", extractExpr), []any{float64(v)}
	case int64:
		return fmt.Sprintf("CAST(%s AS REAL) = ?", extractExpr), []any{float64(v)}
	default:
		// String comparison
		return fmt.Sprintf("%s = ?", extractExpr), []any{fmt.Sprintf("%v", v)}
	}
}

// JSONExtract for PostgresDriver returns the SQL expression to extract a value from a JSON column.
func (d *PostgresDriver) JSONExtract(column, path string) string {
	// PostgreSQL: For nested paths, use #>> operator with array
	// "order.customer.id" becomes input_data::jsonb #>> '{order,customer,id}'
	parts := strings.Split(path, ".")
	pathArray := "{" + strings.Join(parts, ",") + "}"
	return fmt.Sprintf("(%s::jsonb #>> '%s')", column, pathArray)
}

// JSONCompare for PostgresDriver returns the SQL expression to compare a JSON-extracted value.
func (d *PostgresDriver) JSONCompare(extractExpr string, value any, placeholderNum int) (expr string, compArgs []any) {
	switch v := value.(type) {
	case nil:
		return fmt.Sprintf("%s IS NULL", extractExpr), nil
	case bool:
		if v {
			return fmt.Sprintf("%s = 'true'", extractExpr), nil
		}
		return fmt.Sprintf("%s = 'false'", extractExpr), nil
	case float64:
		return fmt.Sprintf("CAST(%s AS NUMERIC) = $%d", extractExpr, placeholderNum), []any{v}
	case int:
		return fmt.Sprintf("CAST(%s AS NUMERIC) = $%d", extractExpr, placeholderNum), []any{v}
	case int64:
		return fmt.Sprintf("CAST(%s AS NUMERIC) = $%d", extractExpr, placeholderNum), []any{v}
	default:
		return fmt.Sprintf("%s = $%d", extractExpr, placeholderNum), []any{fmt.Sprintf("%v", v)}
	}
}

// JSONExtract for MySQLDriver returns the SQL expression to extract a value from a JSON column.
func (d *MySQLDriver) JSONExtract(column, path string) string {
	// MySQL uses JSON_UNQUOTE(JSON_EXTRACT(...)) with $ notation
	return fmt.Sprintf("JSON_UNQUOTE(JSON_EXTRACT(%s, '$.%s'))", column, path)
}

// JSONCompare for MySQLDriver returns the SQL expression to compare a JSON-extracted value.
func (d *MySQLDriver) JSONCompare(extractExpr string, value any, _ int) (expr string, compArgs []any) {
	switch v := value.(type) {
	case nil:
		return fmt.Sprintf("(%s IS NULL OR %s = 'null')", extractExpr, extractExpr), nil
	case bool:
		if v {
			return fmt.Sprintf("(%s = 'true' OR %s = '1')", extractExpr, extractExpr), nil
		}
		return fmt.Sprintf("(%s = 'false' OR %s = '0')", extractExpr, extractExpr), nil
	case float64:
		return fmt.Sprintf("CAST(%s AS DECIMAL(20,6)) = ?", extractExpr), []any{v}
	case int:
		return fmt.Sprintf("CAST(%s AS DECIMAL(20,6)) = ?", extractExpr), []any{float64(v)}
	case int64:
		return fmt.Sprintf("CAST(%s AS DECIMAL(20,6)) = ?", extractExpr), []any{float64(v)}
	default:
		return fmt.Sprintf("%s = ?", extractExpr), []any{fmt.Sprintf("%v", v)}
	}
}
