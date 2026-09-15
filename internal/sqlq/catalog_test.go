package sqlq

import (
	"context"
	"strings"
	"testing"
)

// mapCatalog is an in-memory Catalog for tests: a map from canonical table name to its
// canonical column names.
type mapCatalog map[string][]string

func (m mapCatalog) Tables(ctx context.Context) ([]string, error) {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names, nil
}

func (m mapCatalog) Columns(ctx context.Context, table string) ([]string, error) {
	return m[table], nil
}

// testCatalog is the schema shared by resolution and translation tests.
var testCatalog = mapCatalog{
	"users":  {"id", "name", "status", "age", "email"},
	"orders": {"id", "uid", "amount", "note"},
}

// mustResolve parses and resolves s against testCatalog, failing the test on any error.
func mustResolve(t *testing.T, s string) *Query {
	t.Helper()
	q, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	if err := Resolve(context.Background(), testCatalog, q); err != nil {
		t.Fatalf("Resolve(%q): %v", s, err)
	}
	return q
}

// TestResolveAccepts checks that valid identifier references resolve.
func TestResolveAccepts(t *testing.T) {
	accepts := []string{
		`SELECT * FROM users`,
		`SELECT id, name FROM users`,
		`SELECT u.id FROM users u`,
		`SELECT COUNT(*) FROM users`,
		`SELECT SUM(amount) FROM orders`,
		`SELECT u.name, o.amount FROM users u JOIN orders o ON o.uid = u.id`,
		`SELECT status, COUNT(*) AS n FROM users GROUP BY status ORDER BY n DESC`,
		`SELECT id FROM users WHERE age > 18 AND email IS NOT NULL`,
	}
	for _, s := range accepts {
		q, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if err := Resolve(context.Background(), testCatalog, q); err != nil {
			t.Errorf("Resolve(%q): unexpected error: %v", s, err)
		}
	}
}

// TestResolveRejects checks that references to unknown, ambiguous, or out-of-scope
// identifiers fail with a helpful message.
func TestResolveRejects(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT * FROM nope`, "unknown table"},
		{`SELECT nope FROM users`, "unknown column"},
		{`SELECT u.nope FROM users u`, "unknown column"},
		{`SELECT x.id FROM users u`, "unknown table qualifier"},
		{`SELECT id FROM users u JOIN orders o ON o.uid = u.id`, "ambiguous column"},
		{`SELECT id FROM users JOIN orders ON orders.uid = users.id`, "ambiguous column"},
		{`SELECT u.id FROM users u JOIN orders u ON u.uid = u.id`, "duplicate table"},
		{`SELECT name FROM orders`, "unknown column"},
	}
	for _, c := range cases {
		q, err := Parse(c.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.in, err)
		}
		err = Resolve(context.Background(), testCatalog, q)
		if err == nil {
			t.Errorf("Resolve(%q) = nil, want error containing %q", c.in, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Resolve(%q) error = %q, want to contain %q", c.in, err.Error(), c.want)
		}
	}
}

// TestResolveCanonicalizes checks that identifiers are rewritten to the catalog's own
// casing, which is what lets a query written in any case address a case-sensitive schema.
func TestResolveCanonicalizes(t *testing.T) {
	cat := mapCatalog{"Users": {"Id", "Name"}}
	q, err := Parse(`SELECT name FROM USERS WHERE ID = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if err := Resolve(context.Background(), cat, q); err != nil {
		t.Fatal(err)
	}
	sql, _, err := Translate(q, EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "Users"."Name" FROM "Users" WHERE ("Users"."Id" = $1)`
	if sql != want {
		t.Errorf("canonicalized SQL = %q, want %q", sql, want)
	}
}

// TestResolveAliasReference checks that ORDER BY and GROUP BY may reference a select-list
// alias, which is emitted as a bare identifier.
func TestResolveAliasReference(t *testing.T) {
	q := mustResolve(t, `SELECT status, COUNT(*) AS n FROM users GROUP BY status ORDER BY n DESC`)
	sql, _, err := Translate(q, EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `ORDER BY "n" DESC`) {
		t.Errorf("alias reference not emitted as bare identifier: %s", sql)
	}
}
