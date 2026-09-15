package sqlq

import (
	"strings"
	"testing"
)

// FuzzParse asserts the invariants that make the parser a safety boundary: it never
// panics on any input, anything it accepts translates to a single SELECT statement —
// never a write and never a stacked statement — and a parsed LIMIT is always emitted.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`SELECT * FROM users`,
		`SELECT a, COUNT(*) FROM t GROUP BY a HAVING x`,
		`SELECT a FROM t WHERE b IN (1,2) AND c LIKE 'x%' OR NOT d IS NULL`,
		`SELECT a FROM t u JOIN v w ON w.k = u.k`,
		`INSERT INTO t VALUES (1)`,
		`SELECT 1; DROP TABLE t`,
		`SELECT a FROM t -- comment`,
		`SELECT a FROM (SELECT b FROM t)`,
		`''`,
		`SELECT a FROM t WHERE x = -1.5e3 BETWEEN`,
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		q, err := Parse(s)
		if err != nil {
			return // rejected inputs are fine; we only require no panic
		}
		for _, engine := range []Engine{EnginePostgres, EngineMySQL, EngineSQLite} {
			sql, _, terr := Translate(q, engine)
			if terr != nil {
				continue
			}
			if !strings.HasPrefix(sql, "SELECT ") && sql != "SELECT *" {
				t.Fatalf("accepted input %q translated to non-SELECT: %q", s, sql)
			}
			for _, forbidden := range []string{";", "--", "/*"} {
				if strings.Contains(sql, forbidden) {
					t.Fatalf("accepted input %q translated to SQL containing %q: %q", s, forbidden, sql)
				}
			}
			if q.Limit != nil && !strings.Contains(sql, " LIMIT ") {
				t.Fatalf("accepted input %q lost its LIMIT: %q", s, sql)
			}
		}
	})
}
