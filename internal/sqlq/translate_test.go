package sqlq

import (
	"fmt"
	"strings"
	"testing"
)

// argStrings renders bound arguments as strings so a golden comparison is not brittle about
// the exact numeric Go type the builder chose.
func argStrings(args []any) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = fmt.Sprint(a)
	}
	return out
}

// TestTranslateGolden asserts the exact prepared SQL and bound arguments per dialect. The
// placeholders in the SQL and the arguments carrying every literal together prove that
// values are bound, never interpolated, and that identifiers are quoted per dialect.
func TestTranslateGolden(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		engine  Engine
		wantSQL string
		wantArg []string
	}{
		{
			name:    "star postgres",
			query:   `SELECT * FROM users`,
			engine:  EnginePostgres,
			wantSQL: `SELECT * FROM "users"`,
			wantArg: nil,
		},
		{
			name:    "aggregate group order postgres",
			query:   `SELECT name, COUNT(*) AS cnt FROM users GROUP BY name ORDER BY cnt DESC LIMIT 10`,
			engine:  EnginePostgres,
			wantSQL: `SELECT "users"."name", COUNT(*) AS "cnt" FROM "users" GROUP BY "users"."name" ORDER BY "cnt" DESC LIMIT $1`,
			wantArg: []string{"10"},
		},
		{
			name:    "join postgres",
			query:   `SELECT u.name, o.amount FROM users u INNER JOIN orders o ON o.uid = u.id WHERE u.status = 'active'`,
			engine:  EnginePostgres,
			wantSQL: `SELECT "u"."name", "o"."amount" FROM "users" AS "u" INNER JOIN "orders" AS "o" ON ("o"."uid" = "u"."id") WHERE ("u"."status" = $1)`,
			wantArg: []string{"active"},
		},
		{
			name:    "predicates postgres",
			query:   `SELECT id FROM users WHERE id IN (1, 2, 3) AND name LIKE 'A%' AND age BETWEEN 18 AND 65`,
			engine:  EnginePostgres,
			wantSQL: `SELECT "users"."id" FROM "users" WHERE ((("users"."id" IN ($1, $2, $3)) AND ("users"."name" LIKE $4)) AND ("users"."age" BETWEEN $5 AND $6))`,
			wantArg: []string{"1", "2", "3", "A%", "18", "65"},
		},
		{
			name:    "not de morgan postgres",
			query:   `SELECT id FROM users WHERE NOT (status = 'active' AND age > 18)`,
			engine:  EnginePostgres,
			wantSQL: `SELECT "users"."id" FROM "users" WHERE (("users"."status" != $1) OR ("users"."age" <= $2))`,
			wantArg: []string{"active", "18"},
		},
		{
			name:    "quoting mysql",
			query:   `SELECT id, name FROM users WHERE id = 1`,
			engine:  EngineMySQL,
			wantSQL: "SELECT `users`.`id`, `users`.`name` FROM `users` WHERE (`users`.`id` = ?)",
			wantArg: []string{"1"},
		},
		{
			name:    "quoting sqlite",
			query:   `SELECT id FROM users WHERE age >= 18 LIMIT 5 OFFSET 10`,
			engine:  EngineSQLite,
			wantSQL: "SELECT `users`.`id` FROM `users` WHERE (`users`.`age` >= ?) LIMIT ? OFFSET ?",
			wantArg: []string{"18", "5", "10"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := mustResolve(t, c.query)
			sql, args, err := Translate(q, c.engine)
			if err != nil {
				t.Fatal(err)
			}
			if sql != c.wantSQL {
				t.Errorf("SQL:\n got %q\nwant %q", sql, c.wantSQL)
			}
			got := argStrings(args)
			if strings.Join(got, "|") != strings.Join(c.wantArg, "|") {
				t.Errorf("args = %v, want %v", got, c.wantArg)
			}
		})
	}
}

// TestTranslateBindsNoLiterals checks that a string value never appears in the SQL text: it
// must be a bound parameter. This is the property the security review turns on.
func TestTranslateBindsNoLiterals(t *testing.T) {
	q := mustResolve(t, `SELECT id FROM users WHERE name = 'sekret' AND status IN ('a', 'b')`)
	for _, e := range []Engine{EnginePostgres, EngineMySQL, EngineSQLite} {
		sql, args, err := Translate(q, e)
		if err != nil {
			t.Fatal(err)
		}
		for _, lit := range []string{"sekret", "'a'", "'b'"} {
			if strings.Contains(sql, lit) {
				t.Errorf("[%s] SQL contains interpolated literal %q: %s", e, lit, sql)
			}
		}
		if len(args) != 3 {
			t.Errorf("[%s] args = %v, want 3 bound values", e, args)
		}
	}
}

// TestTranslateColumnComparison checks that a column-to-column comparison emits two quoted
// identifiers and binds nothing, so join conditions carry no client text.
func TestTranslateColumnComparison(t *testing.T) {
	q := mustResolve(t, `SELECT u.id FROM users u JOIN orders o ON o.uid = u.id WHERE o.amount > u.age`)
	sql, args, err := Translate(q, EnginePostgres)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, `("o"."amount" > "u"."age")`) {
		t.Errorf("column comparison not emitted as identifiers: %s", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// TestTranslateRejectsBadIdentifiers checks that Translate refuses a query whose
// identifiers were not checked, because the builder does not escape quotes in them.
func TestTranslateRejectsBadIdentifiers(t *testing.T) {
	bad := `id" FROM users; DROP TABLE x --`
	cases := map[string]*Query{
		"column": {Columns: []SelectItem{{Column: Column{Table: "users", Name: bad}}}, From: TableRef{Name: "users"}},
		"table":  {Star: true, From: TableRef{Name: bad}},
		"alias":  {Columns: []SelectItem{{Column: Column{Table: "users", Name: "id"}, Alias: bad}}, From: TableRef{Name: "users"}},
		"where": {Star: true, From: TableRef{Name: "users"},
			Where: Comparison{Col: Column{Table: "users", Name: bad}, Op: "=", Val: Value{Go: 1}}},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			sql, _, err := Translate(q, EnginePostgres)
			if err == nil || !strings.Contains(err.Error(), "invalid identifier") {
				t.Fatalf("err = %v (sql %q), want an invalid identifier error", err, sql)
			}
		})
	}
}

// TestTranslateRejectsZeroLimit checks that a LIMIT of zero is refused instead of being
// dropped by the builder.
func TestTranslateRejectsZeroLimit(t *testing.T) {
	zero := 0
	q := &Query{Star: true, From: TableRef{Name: "users"}, Limit: &zero}
	if _, _, err := Translate(q, EnginePostgres); err == nil {
		t.Fatal("want an error for LIMIT 0")
	}
}
