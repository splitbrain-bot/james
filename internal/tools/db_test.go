package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"james/internal/agent"
	"james/internal/config"
)

// makeTestDB writes a small test database with users and orders and returns its path.
func makeTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, city TEXT)`,
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, amount REAL NOT NULL)`,
		`INSERT INTO users (id, name, city) VALUES (1, 'Anna', 'Berlin'), (2, 'Bert', NULL), (3, 'Cleo', 'Kiel')`,
		`INSERT INTO orders (id, user_id, amount) VALUES (1, 1, 10.0), (2, 1, 20.0), (3, 2, 5.0), (4, 3, 7.0), (5, 3, 8.0)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("setup %q: %v", s, err)
		}
	}
	return path
}

// makeDBTools opens the tools for the given databases and closes them afterwards.
func makeDBTools(t *testing.T, dbs map[string]config.Database) (schema, query agent.Tool) {
	t.Helper()
	tools, closeFn, err := NewDBTools(context.Background(), dbs)
	if err != nil {
		t.Fatalf("NewDBTools: %v", err)
	}
	t.Cleanup(func() { _ = closeFn.Close() })
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	return tools[0], tools[1]
}

// oneTestDB returns the configuration of a single test database.
func oneTestDB(t *testing.T, maxRows int, timeout time.Duration) map[string]config.Database {
	t.Helper()
	return map[string]config.Database{
		"main": {Driver: "sqlite", DSN: makeTestDB(t), MaxRows: maxRows, Timeout: timeout},
	}
}

// runDB calls a tool with the given input object and fails on a tool error.
func runDB(t *testing.T, tool agent.Tool, input string) string {
	t.Helper()
	res, err := tool.Run(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s(%s): %v", tool.Name(), input, err)
	}
	return res.Text
}

// runDBErr calls a tool with the given input object and expects an error.
func runDBErr(t *testing.T, tool agent.Tool, input string) string {
	t.Helper()
	res, err := tool.Run(context.Background(), json.RawMessage(input))
	if err == nil {
		t.Fatalf("%s(%s): want an error, got %q", tool.Name(), input, res.Text)
	}
	return err.Error()
}

// TestDBSchemaLists checks that the listing names every table with its column types and no nullability.
func TestDBSchemaLists(t *testing.T) {
	schema, _ := makeDBTools(t, oneTestDB(t, 0, 0))
	if schema.Name() != "db_schema" {
		t.Fatalf("got name %q", schema.Name())
	}

	out := runDB(t, schema, `{}`)
	for _, want := range []string{"users", "orders", "user_id", "INTEGER", "TEXT", "REAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT NULL") {
		t.Errorf("listing should not show nullability:\n%s", out)
	}
}

// TestDBSchemaOneTable checks that a named table is shown alone and with its nullability.
func TestDBSchemaOneTable(t *testing.T) {
	schema, _ := makeDBTools(t, oneTestDB(t, 0, 0))

	out := runDB(t, schema, `{"connection":"main","table":"users"}`)
	if strings.Contains(out, "orders") {
		t.Errorf("one table asked, other table shown:\n%s", out)
	}
	for _, want := range []string{"name TEXT NOT NULL", "city TEXT NULL"} {
		if !strings.Contains(out, want) {
			t.Errorf("table misses %q:\n%s", want, out)
		}
	}
}

// TestDBSchemaUnknownTable checks the error for a table the database does not have.
func TestDBSchemaUnknownTable(t *testing.T) {
	schema, _ := makeDBTools(t, oneTestDB(t, 0, 0))

	msg := runDBErr(t, schema, `{"table":"nope"}`)
	if !strings.Contains(msg, `unknown table "nope"`) {
		t.Errorf("got error %q", msg)
	}
}

// TestDBQueryJoinAndAggregate checks a join with grouping, aggregates and ordering, rendered as a table.
func TestDBQueryJoinAndAggregate(t *testing.T) {
	_, query := makeDBTools(t, oneTestDB(t, 0, 0))
	if query.Name() != "db_query" {
		t.Fatalf("got name %q", query.Name())
	}

	out := runDB(t, query, `{"query":"SELECT u.name, COUNT(*) AS n, SUM(o.amount) AS total `+
		`FROM users u JOIN orders o ON o.user_id = u.id WHERE u.name <> 'Bert' `+
		`GROUP BY u.name ORDER BY n DESC, u.name"}`)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	want := []string{
		"name\tn\ttotal",
		"Anna\t2\t30",
		"Cleo\t2\t15",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), out)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d is %q, want %q", i, lines[i], w)
		}
	}
}

// TestDBQueryNoRows checks the answer for a query that matches nothing.
func TestDBQueryNoRows(t *testing.T) {
	_, query := makeDBTools(t, oneTestDB(t, 0, 0))

	out := runDB(t, query, `{"query":"SELECT id FROM users WHERE name = 'nobody'"}`)
	if !strings.Contains(out, "no rows") {
		t.Errorf("got %q", out)
	}
}

// TestDBQueryRowCap checks the configured row cap, its note, and a smaller query limit passing uncapped.
func TestDBQueryRowCap(t *testing.T) {
	_, query := makeDBTools(t, oneTestDB(t, 2, 0))

	for _, q := range []string{"SELECT id FROM orders", "SELECT id FROM orders LIMIT 100", "SELECT id FROM orders LIMIT 0"} {
		out := runDB(t, query, fmt.Sprintf(`{"query":%q}`, q))
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != 4 {
			t.Fatalf("%s: got %d lines, want header, 2 rows and a note:\n%s", q, len(lines), out)
		}
		if !strings.Contains(out, "row limit of 2 rows") {
			t.Errorf("%s: note missing:\n%s", q, out)
		}
	}

	out := runDB(t, query, `{"query":"SELECT id FROM orders LIMIT 1"}`)
	if strings.Contains(out, "row limit") {
		t.Errorf("unexpected note:\n%s", out)
	}
}

// TestDBQueryRejectsWrites checks that writes and stacked statements fail as grammar errors, with the data intact.
func TestDBQueryRejectsWrites(t *testing.T) {
	_, query := makeDBTools(t, oneTestDB(t, 0, 0))

	queries := []string{
		"DELETE FROM users",
		"SELECT * FROM users INTO OUTFILE '/tmp/out'",
		"WITH x AS (SELECT id FROM users) DELETE FROM users",
		"UPDATE users SET name = 'x'",
		"SELECT id FROM users; DROP TABLE users",
	}
	for _, q := range queries {
		msg := runDBErr(t, query, fmt.Sprintf(`{"query":%q}`, q))
		if !strings.Contains(msg, "does not fit the grammar") {
			t.Errorf("%s: got error %q", q, msg)
		}
	}

	out := runDB(t, query, `{"query":"SELECT COUNT(*) AS n FROM users"}`)
	if !strings.Contains(out, "3") {
		t.Errorf("rows changed:\n%s", out)
	}
}

// TestDBQueryUnknownNames checks the errors for an unknown table and an unknown column.
func TestDBQueryUnknownNames(t *testing.T) {
	_, query := makeDBTools(t, oneTestDB(t, 0, 0))

	if msg := runDBErr(t, query, `{"query":"SELECT id FROM nope"}`); !strings.Contains(msg, `unknown table "nope"`) {
		t.Errorf("got error %q", msg)
	}
	if msg := runDBErr(t, query, `{"query":"SELECT nope FROM users"}`); !strings.Contains(msg, `unknown column "nope"`) {
		t.Errorf("got error %q", msg)
	}
}

// TestDBSeveralConnections checks the required connection name, its errors and both tool descriptions.
func TestDBSeveralConnections(t *testing.T) {
	dbs := map[string]config.Database{
		"main":  {Driver: "sqlite", DSN: makeTestDB(t)},
		"other": {Driver: "sqlite", DSN: makeTestDB(t)},
	}
	schema, query := makeDBTools(t, dbs)

	if !strings.Contains(string(query.Schema()), `"required":["query","connection"]`) {
		t.Errorf("connection not required: %s", query.Schema())
	}
	if msg := runDBErr(t, schema, `{}`); !strings.Contains(msg, "main, other") {
		t.Errorf("got error %q", msg)
	}
	if msg := runDBErr(t, query, `{"connection":"gone","query":"SELECT id FROM users"}`); !strings.Contains(msg, `unknown connection "gone"`) {
		t.Errorf("got error %q", msg)
	}
	if out := runDB(t, query, `{"connection":"other","query":"SELECT COUNT(*) AS n FROM users"}`); !strings.Contains(out, "3") {
		t.Errorf("got %q", out)
	}

	for _, tool := range []agent.Tool{schema, query} {
		for _, want := range []string{"main (sqlite)", "other (sqlite)"} {
			if !strings.Contains(tool.Description(), want) {
				t.Errorf("%s description misses %q", tool.Name(), want)
			}
		}
	}
}

// TestDBOpenFails checks that a missing database file and an unknown driver fail at startup.
func TestDBOpenFails(t *testing.T) {
	dbs := map[string]config.Database{
		"gone": {Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "missing.db")},
	}
	_, _, err := NewDBTools(context.Background(), dbs)
	if err == nil || !strings.Contains(err.Error(), `database "gone"`) {
		t.Fatalf("got error %v", err)
	}

	bad := map[string]config.Database{"x": {Driver: "oracle", DSN: "whatever"}}
	if _, _, err := NewDBTools(context.Background(), bad); err == nil ||
		!strings.Contains(err.Error(), "unknown driver") {
		t.Fatalf("got error %v", err)
	}
}

// TestDBNoDatabases checks that an empty configuration yields no tools and a working close function.
func TestDBNoDatabases(t *testing.T) {
	tools, closeFn, err := NewDBTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewDBTools: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("got %d tools, want none", len(tools))
	}
	if err := closeFn.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// TestDBCapText checks text below the cap and a cut that ends on a whole character.
func TestDBCapText(t *testing.T) {
	if s, cut := capText("hello", 10); cut || s != "hello" {
		t.Errorf("got %q, %v", s, cut)
	}
	if s, cut := capText("aä", 2); !cut || s != "a" {
		t.Errorf("got %q, %v", s, cut)
	}
}

// TestDBMySQLDSN checks that the hardened DSN keeps multiple statements and
// local file reads off, even when the configured DSN asks for them.
func TestDBMySQLDSN(t *testing.T) {
	for _, dsn := range []string{
		"user:pw@tcp(db:3306)/app",
		"user:pw@tcp(db:3306)/app?multiStatements=true&allowAllFiles=true",
	} {
		hardened, err := dbMySQLDSN(dsn)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		cfg, err := mysql.ParseDSN(hardened)
		if err != nil {
			t.Fatalf("%s: %v", hardened, err)
		}
		if cfg.MultiStatements || cfg.AllowAllFiles {
			t.Errorf("%s became %s with multiStatements=%v and allowAllFiles=%v",
				dsn, hardened, cfg.MultiStatements, cfg.AllowAllFiles)
		}
	}
}
