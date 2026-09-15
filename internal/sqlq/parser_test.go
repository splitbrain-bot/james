package sqlq

import (
	"strings"
	"testing"
)

// TestParseAccepts checks that well-formed queries across the whole grammar parse. These
// guard against over-restriction: the features below are the ones the tool must support.
func TestParseAccepts(t *testing.T) {
	accepts := []string{
		`SELECT * FROM users`,
		`SELECT id, name FROM users`,
		`SELECT id FROM users AS u`,
		`SELECT id FROM users u`,
		`SELECT u.id, u.name FROM users u`,
		`SELECT COUNT(*) FROM users`,
		`SELECT COUNT(*) AS n FROM users`,
		`SELECT SUM(amount), AVG(amount), MIN(amount), MAX(amount) FROM orders`,
		`SELECT status, COUNT(*) AS n FROM orders GROUP BY status ORDER BY n DESC`,
		`SELECT u.name, o.amount FROM users u INNER JOIN orders o ON o.uid = u.id`,
		`SELECT u.name FROM users u LEFT JOIN orders o ON o.uid = u.id`,
		`SELECT u.name FROM users u LEFT OUTER JOIN orders o ON o.uid = u.id`,
		`SELECT u.name FROM users u RIGHT JOIN orders o ON o.uid = u.id`,
		`SELECT u.name FROM users u JOIN orders o ON o.uid = u.id`,
		`SELECT id FROM users WHERE age > 18`,
		`SELECT id FROM users WHERE age >= 18 AND age <= 65`,
		`SELECT id FROM users WHERE name = 'alice' OR name = 'bob'`,
		`SELECT id FROM users WHERE NOT age > 18`,
		`SELECT id FROM users WHERE status IN ('active', 'pending')`,
		`SELECT id FROM users WHERE id IN (1, 2, 3)`,
		`SELECT id FROM users WHERE name LIKE 'a%'`,
		`SELECT id FROM users WHERE email IS NULL`,
		`SELECT id FROM users WHERE email IS NOT NULL`,
		`SELECT id FROM users WHERE age BETWEEN 18 AND 65`,
		`SELECT id FROM users WHERE (age > 18 AND age < 65) OR status = 'vip'`,
		`SELECT id FROM users WHERE NOT (age > 18 AND status = 'x')`,
		`SELECT id FROM users WHERE balance = -100`,
		`SELECT id FROM users WHERE rate < 3.14`,
		`SELECT id FROM users WHERE rate < 1.5e3`,
		`SELECT id FROM users WHERE flag = TRUE`,
		`SELECT id FROM users WHERE flag = false`,
		`SELECT id FROM users WHERE a = b`,
		`SELECT id FROM users LIMIT 10`,
		`SELECT id FROM users LIMIT 10 OFFSET 20`,
		`SELECT id FROM users ORDER BY name`,
		`SELECT id FROM users ORDER BY name ASC, id DESC`,
		`select ID from USERS where NAME = 'x'`, // case-insensitive keywords
		`SELECT id FROM users WHERE note = 'it''s here'`,
	}
	for _, s := range accepts {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", s, err)
		}
	}
}

// TestParseRejects is the core security test: every write, statement-stacking, comment,
// subquery, unknown-function, and malformed construct must be a parse error. Read-only is a
// property of the grammar, so a write must be unrepresentable, not merely denied.
func TestParseRejects(t *testing.T) {
	rejects := []string{
		// Write verbs are not keywords, so they cannot begin a query.
		`INSERT INTO users (id) VALUES (1)`,
		`UPDATE users SET name = 'x'`,
		`DELETE FROM users`,
		`DROP TABLE users`,
		`TRUNCATE users`,
		`ALTER TABLE users ADD COLUMN x INT`,
		`CREATE TABLE t (id INT)`,
		`GRANT ALL ON users TO bob`,
		`REPLACE INTO users VALUES (1)`,
		`MERGE INTO users`,
		`CALL do_something()`,
		`COPY users TO '/tmp/x'`,
		// Statement stacking: ";" is a lexical error, so a single statement is structural.
		`SELECT id FROM users; DROP TABLE users`,
		`SELECT id FROM users;`,
		`SELECT id FROM users; SELECT 1`,
		// Comments have no differential to exploit because their characters are illegal.
		`SELECT id FROM users -- comment`,
		`SELECT id FROM users # comment`,
		`SELECT id /* c */ FROM users`,
		`SELECT id FROM users /*! MYSQL */`,
		// Subqueries, set operations, and CTEs are not in the grammar.
		`SELECT * FROM (SELECT id FROM users) x`,
		`SELECT id FROM users WHERE id IN (SELECT id FROM admins)`,
		`SELECT id FROM users UNION SELECT id FROM admins`,
		`WITH x AS (SELECT 1) SELECT * FROM x`,
		// Only the five aggregates are nameable; no other function is.
		`SELECT LOWER(name) FROM users`,
		`SELECT pg_sleep(10) FROM users`,
		`SELECT version() FROM users`,
		`SELECT COUNT(id, name) FROM users`,
		`SELECT SUM(*) FROM users`,
		// CASE and HAVING are excluded in v1.
		`SELECT CASE WHEN a THEN 1 END FROM users`,
		`SELECT id FROM users GROUP BY id HAVING COUNT(*) > 1`,
		// Malformed predicates and clauses.
		`SELECT id FROM users WHERE age == 1`,
		`SELECT id FROM users WHERE age`,
		`SELECT id FROM users WHERE LIKE 'x'`,
		`SELECT id FROM users WHERE name LIKE 5`,
		`SELECT id FROM users WHERE age BETWEEN 1`,
		`SELECT id FROM users WHERE age IN ()`,
		`SELECT FROM users`,
		`SELECT id users`,
		`SELECT id FROM`,
		`SELECT id FROM users WHERE`,
		`SELECT id FROM users LIMIT`,
		`SELECT id FROM users LIMIT -1`,
		`SELECT id FROM users LIMIT 1.5`,
		`SELECT id FROM users alias extra`,
		`SELECT * , id FROM users`,
		`SELECT id FROM users JOIN orders`,    // missing ON
		`SELECT id FROM users JOIN orders ON`, // missing condition
		`SELECT "id" FROM users`,              // double-quoted identifiers not allowed
		"SELECT `id` FROM users",              // backtick identifiers not allowed
		``,                                    // empty
		`   `,                                 // whitespace only
	}
	for _, s := range rejects {
		if q, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = %+v, want error", s, q)
		}
	}
}

// TestParseStructure checks that the AST reflects the source for a representative query.
func TestParseStructure(t *testing.T) {
	q, err := Parse(`SELECT u.name, COUNT(*) AS n FROM users u LEFT JOIN orders o ON o.uid = u.id WHERE u.age >= 18 GROUP BY u.name ORDER BY n DESC LIMIT 5 OFFSET 10`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Star {
		t.Error("Star should be false")
	}
	if len(q.Columns) != 2 {
		t.Fatalf("Columns = %d, want 2", len(q.Columns))
	}
	if q.Columns[1].Agg != "COUNT" || !q.Columns[1].Star || q.Columns[1].Alias != "n" {
		t.Errorf("second column = %+v, want COUNT(*) AS n", q.Columns[1])
	}
	if q.From.Name != "users" || q.From.Alias != "u" {
		t.Errorf("From = %+v", q.From)
	}
	if len(q.Joins) != 1 || q.Joins[0].Type != LeftJoin {
		t.Errorf("Joins = %+v", q.Joins)
	}
	if q.Limit == nil || *q.Limit != 5 {
		t.Errorf("Limit = %v, want 5", q.Limit)
	}
	if q.Offset == nil || *q.Offset != 10 {
		t.Errorf("Offset = %v, want 10", q.Offset)
	}
	if len(q.OrderBy) != 1 || !q.OrderBy[0].Desc {
		t.Errorf("OrderBy = %+v", q.OrderBy)
	}
}

// TestParseErrorMessages checks that a few errors are described helpfully, since the client
// agent relies on the message to correct its query.
func TestParseErrorMessages(t *testing.T) {
	cases := []struct{ in, want string }{
		{`DELETE FROM users`, "expected SELECT"},
		{`SELECT id FROM users;`, "unexpected character"},
		{`SELECT LOWER(x) FROM users`, "the grammar has no function LOWER"},
		{`SELECT id FROM users WHERE name LIKE 5`, "LIKE requires a string"},
		{`SELECT DISTINCT id FROM users`, "the grammar has no DISTINCT"},
		{`SELECT id FROM users GROUP BY id HAVING COUNT(*) > 1`, "the grammar has no HAVING"},
		{`SELECT id FROM users UNION SELECT id FROM admins`, "the grammar has no UNION"},
		{`WITH x AS (SELECT 1) SELECT * FROM x`, "the grammar has no WITH"},
		{`SELECT CASE WHEN a THEN 1 END FROM users`, "the grammar has no CASE"},
		{`SELECT a + b FROM users`, "the grammar has no computed expressions"},
		{`SELECT a / b FROM users`, "the grammar has no computed expressions"},
		{`SELECT id FROM users -- comment`, "comments are not allowed"},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		if err == nil {
			t.Errorf("Parse(%q) = nil error", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q) error = %q, want to contain %q", c.in, err.Error(), c.want)
		}
	}
}
