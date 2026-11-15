package db

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
)

type schemaQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type readableColumn struct {
	notNull bool
	key     bool
}

// schemaReader is the shared projection boundary for observational queries.
// Column names come from the read model, while their availability comes from
// this connection's schema. No migration or schema cache is needed to read a
// database left open by an older daemon.
type schemaReader struct {
	table    string
	columns  map[string]readableColumn
	required map[string]bool
}

func readSchema(ctx context.Context, q schemaQueryer, table string, required ...string) (*schemaReader, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, "notnull", pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("read %s schema: %w", table, err)
	}
	defer rows.Close()
	r := &schemaReader{table: table, columns: make(map[string]readableColumn), required: make(map[string]bool)}
	for rows.Next() {
		var name string
		var notNull, key int
		if err := rows.Scan(&name, &notNull, &key); err != nil {
			return nil, fmt.Errorf("read %s schema: %w", table, err)
		}
		r.columns[name] = readableColumn{notNull: notNull != 0, key: key != 0}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s schema: %w", table, err)
	}
	if len(r.columns) == 0 {
		return nil, fmt.Errorf("read %s schema: table is missing", table)
	}
	for _, name := range required {
		if _, ok := r.columns[name]; !ok {
			return nil, fmt.Errorf("read %s schema: required column %q is missing", table, name)
		}
		r.required[name] = true
	}
	return r, nil
}

// projection shares the scan order and destination types with the model's
// scanner. Adding a model field therefore gets legacy-schema handling without
// another list of historical columns or special cases for a particular table.
func (r *schemaReader) projection(names string, destinations ...any) (string, error) {
	columns := strings.Split(names, ",")
	if len(columns) != len(destinations) {
		return "", fmt.Errorf("read %s projection: %d columns for %d destinations", r.table, len(columns), len(destinations))
	}
	for i, name := range columns {
		expression, err := r.expression(strings.TrimSpace(name), destinations[i])
		if err != nil {
			return "", err
		}
		columns[i] = expression
	}
	return strings.Join(columns, ", "), nil
}

func (r *schemaReader) expression(name string, destination any) (string, error) {
	v := reflect.ValueOf(destination)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return "", fmt.Errorf("read %s column %q: scan destination must be a non-nil pointer", r.table, name)
	}
	zero := "NULL"
	switch v.Elem().Kind() {
	case reflect.String:
		zero = "''"
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		zero = "0"
	}
	column, present := r.columns[name]
	if !present {
		return zero, nil
	}
	expression := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	// Nullable storage mapped to a scalar Go field has that field's zero
	// value. Pointer, byte-slice and sql.Null* destinations preserve NULL.
	// Required identity columns retain genuine NULL/scan errors.
	if zero != "NULL" && !column.notNull && !column.key && !r.required[name] {
		expression = "COALESCE(" + expression + ", " + zero + ")"
	}
	return expression, nil
}

func (d *DB) readProjection(table, names string, destinations ...any) (string, error) {
	r, err := readSchema(context.Background(), d.sql, table, "id")
	if err != nil {
		return "", err
	}
	return r.projection(names, destinations...)
}
