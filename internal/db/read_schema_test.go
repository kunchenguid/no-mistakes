package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSchemaReaderDefaultsFollowScanTypesAcrossTables(t *testing.T) {
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	for _, table := range []string{"first_model", "second_model"} {
		t.Run(table, func(t *testing.T) {
			if _, err := conn.Exec(`CREATE TABLE ` + table + ` (id TEXT PRIMARY KEY, retained TEXT NOT NULL, nullable_text TEXT); INSERT INTO ` + table + ` VALUES ('row', 'committed', NULL)`); err != nil {
				t.Fatal(err)
			}
			type model struct {
				ID, Retained, Added, Nullable string
				Count                         int64
				Enabled                       bool
				Fraction                      float64
				Optional                      *string
				Payload                       []byte
				Unknown                       sql.NullString
			}
			read := func() model {
				t.Helper()
				r, err := readSchema(context.Background(), conn, table, "id")
				if err != nil {
					t.Fatal(err)
				}
				m := model{Added: "stale", Count: 99, Enabled: true}
				targets := []any{&m.ID, &m.Retained, &m.Added, &m.Nullable, &m.Count, &m.Enabled, &m.Fraction, &m.Optional, &m.Payload, &m.Unknown}
				columns, err := r.projection("id, retained, added, nullable_text, count, enabled, fraction, optional, payload, unknown", targets...)
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.QueryRow(`SELECT ` + columns + ` FROM ` + table + ` WHERE id = 'row'`).Scan(targets...); err != nil {
					t.Fatal(err)
				}
				return m
			}
			want := model{ID: "row", Retained: "committed"}
			if got := read(); !reflect.DeepEqual(got, want) {
				t.Fatalf("legacy row = %#v, want %#v", got, want)
			}
			// The reader must observe an additive migration performed by a
			// writer between observations, without a cached schema or reopen.
			if _, err := conn.Exec(`ALTER TABLE ` + table + ` ADD COLUMN added TEXT; UPDATE ` + table + ` SET added = 'now recorded'`); err != nil {
				t.Fatal(err)
			}
			want.Added = "now recorded"
			if got := read(); !reflect.DeepEqual(got, want) {
				t.Fatalf("advanced row = %#v, want %#v", got, want)
			}
		})
	}
}

func TestSchemaReaderDoesNotInventIdentityOrHideReadErrors(t *testing.T) {
	conn, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "invalid.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TABLE missing_identity (payload TEXT); CREATE TABLE rows_with_identity (id TEXT PRIMARY KEY, value TEXT); INSERT INTO rows_with_identity VALUES (NULL, 'broken')`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"missing_table", "missing_identity"} {
		if _, err := readSchema(context.Background(), conn, table, "id"); err == nil {
			t.Fatalf("%s must report its missing storage/identity", table)
		}
	}
	r, err := readSchema(context.Background(), conn, "rows_with_identity", "id")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	columns, err := r.projection("id", &id)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(`SELECT ` + columns + ` FROM rows_with_identity`).Scan(&id); err == nil {
		t.Fatal("a NULL identity must remain a genuine scan error")
	}
	if _, err := r.projection("id, value", &id); err == nil {
		t.Fatal("a model/scan shape mismatch must report an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readSchema(ctx, conn, "rows_with_identity", "id"); err == nil {
		t.Fatal("a failed schema read must not become a zero-valued model")
	}
}
