package sqlq

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// identRe is the syntactic form every identifier must match. The parser only ever produces
// identifiers of this shape; resolution re-checks it as defence in depth before an
// identifier is handed to the query builder.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidIdent reports whether s is a syntactically valid identifier.
func ValidIdent(s string) bool { return identRe.MatchString(s) }

// Querier is the subset of *sql.DB and *sql.Tx that catalog introspection uses. A read-only
// transaction satisfies it, so introspection and the query it informs share one snapshot.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Catalog reports the tables and their columns visible to a connection, so the identifiers
// in a parsed Query can be resolved against the live schema.
type Catalog interface {
	// Tables returns the names of the base tables and views visible to the connection.
	Tables(ctx context.Context) ([]string, error)

	// Columns returns the column names of the named table in schema order.
	Columns(ctx context.Context, table string) ([]string, error)
}

// ColumnInfo describes one column for schema introspection display.
type ColumnInfo struct {
	// Name is the column name.
	Name string

	// Type is the engine's declared type for the column.
	Type string

	// Nullable reports whether the column admits NULL.
	Nullable bool
}

// Resolve validates and resolves every identifier in q against cat, rewriting table and
// column names to their canonical spelling from the catalog. It fails when a table, table
// qualifier, or column is unknown or ambiguous. Because the query builder quotes the
// resolved identifiers per dialect, rewriting to the catalog's own casing is what lets a
// query written in any casing address a case-sensitive schema.
func Resolve(ctx context.Context, cat Catalog, q *Query) error {
	tables, err := cat.Tables(ctx)
	if err != nil {
		return err
	}
	tableSet := make(map[string]string, len(tables))
	for _, t := range tables {
		key := strings.ToLower(t)
		if _, dup := tableSet[key]; !dup {
			tableSet[key] = t
		}
	}

	r := &resolver{scope: map[string]*binding{}}

	// Bind the FROM table and every joined table into scope.
	if err := r.bind(ctx, cat, tableSet, &q.From); err != nil {
		return err
	}
	for i := range q.Joins {
		if err := r.bind(ctx, cat, tableSet, &q.Joins[i].Table); err != nil {
			return err
		}
	}

	// Collect select-list aliases so ORDER BY and GROUP BY may reference them by name.
	r.aliases = map[string]string{}
	for _, item := range q.Columns {
		if item.Alias != "" {
			r.aliases[strings.ToLower(item.Alias)] = item.Alias
		}
	}

	// Resolve the select-list columns: an aggregate's argument, or a plain column. COUNT(*)
	// has no column to resolve.
	for i := range q.Columns {
		item := &q.Columns[i]
		if item.Star {
			continue
		}
		col, err := r.resolveCol(item.Column, false)
		if err != nil {
			return err
		}
		item.Column = col
	}

	// Resolve join conditions.
	for i := range q.Joins {
		cond, err := r.rewriteCondition(q.Joins[i].On)
		if err != nil {
			return err
		}
		q.Joins[i].On = cond
	}

	// Resolve the WHERE condition.
	if q.Where != nil {
		cond, err := r.rewriteCondition(q.Where)
		if err != nil {
			return err
		}
		q.Where = cond
	}

	// Resolve GROUP BY and ORDER BY, allowing references to select-list aliases.
	for i := range q.GroupBy {
		col, err := r.resolveCol(q.GroupBy[i], true)
		if err != nil {
			return err
		}
		q.GroupBy[i] = col
	}
	for i := range q.OrderBy {
		col, err := r.resolveCol(q.OrderBy[i].Column, true)
		if err != nil {
			return err
		}
		q.OrderBy[i].Column = col
	}

	return nil
}

// binding is one table in scope: its canonical name, the name it is addressed by (its alias
// if it has one, otherwise the canonical name), and its columns keyed by lower case.
type binding struct {
	// name is the output qualifier: the alias if present, else the canonical table name.
	name string

	// columns maps a lower-cased column name to its canonical spelling.
	columns map[string]string
}

// resolver holds the scope built from the FROM and JOIN tables plus the select-list
// aliases, and resolves column references against them.
type resolver struct {
	// scope maps a lower-cased binding name to its binding.
	scope map[string]*binding

	// order lists the bindings in the order they were added, for stable error messages
	// and deterministic ambiguity checks.
	order []*binding

	// aliases maps a lower-cased select-list alias to its canonical spelling.
	aliases map[string]string
}

// bind resolves ref against the catalog, rewrites its name to the canonical spelling, and
// adds it to scope. It fails on an unknown table or a duplicate binding name.
func (r *resolver) bind(ctx context.Context, cat Catalog, tableSet map[string]string, ref *TableRef) error {
	if !ValidIdent(ref.Name) {
		return fmt.Errorf("invalid table name %q", ref.Name)
	}
	if ref.Alias != "" && !ValidIdent(ref.Alias) {
		return fmt.Errorf("invalid table alias %q", ref.Alias)
	}
	canonical, ok := tableSet[strings.ToLower(ref.Name)]
	if !ok {
		return fmt.Errorf("unknown table %q", ref.Name)
	}
	ref.Name = canonical

	name := canonical
	if ref.Alias != "" {
		name = ref.Alias
	}
	key := strings.ToLower(name)
	if _, dup := r.scope[key]; dup {
		return fmt.Errorf("duplicate table name or alias %q", name)
	}

	cols, err := cat.Columns(ctx, canonical)
	if err != nil {
		return err
	}
	colMap := make(map[string]string, len(cols))
	for _, c := range cols {
		ckey := strings.ToLower(c)
		if _, dup := colMap[ckey]; !dup {
			colMap[ckey] = c
		}
	}

	b := &binding{name: name, columns: colMap}
	r.scope[key] = b
	r.order = append(r.order, b)
	return nil
}

// resolveCol resolves a column reference to a canonical, qualified column, or — when
// allowAlias is set and the reference names a select-list alias — to an unqualified alias
// reference (Table left empty). It fails on an unknown, unqualified-and-unknown, or
// ambiguous column.
func (r *resolver) resolveCol(col Column, allowAlias bool) (Column, error) {
	if col.Table != "" && !ValidIdent(col.Table) {
		return Column{}, fmt.Errorf("invalid table qualifier %q", col.Table)
	}
	if !ValidIdent(col.Name) {
		return Column{}, fmt.Errorf("invalid column name %q", col.Name)
	}

	if col.Table != "" {
		b, ok := r.scope[strings.ToLower(col.Table)]
		if !ok {
			return Column{}, fmt.Errorf("unknown table qualifier %q", col.Table)
		}
		c, ok := b.columns[strings.ToLower(col.Name)]
		if !ok {
			return Column{}, fmt.Errorf("unknown column %q in table %q", col.Name, b.name)
		}
		return Column{Table: b.name, Name: c}, nil
	}

	if allowAlias {
		if canonical, ok := r.aliases[strings.ToLower(col.Name)]; ok {
			return Column{Name: canonical}, nil
		}
	}

	var found *binding
	var canonical string
	for _, b := range r.order {
		if c, ok := b.columns[strings.ToLower(col.Name)]; ok {
			if found != nil {
				return Column{}, fmt.Errorf("ambiguous column %q; qualify it with a table", col.Name)
			}
			found = b
			canonical = c
		}
	}
	if found == nil {
		return Column{}, fmt.Errorf("unknown column %q", col.Name)
	}
	return Column{Table: found.name, Name: canonical}, nil
}

// rewriteCondition returns a copy of c with every column reference resolved against scope.
func (r *resolver) rewriteCondition(c Condition) (Condition, error) {
	switch v := c.(type) {
	case Logical:
		left, err := r.rewriteCondition(v.Left)
		if err != nil {
			return nil, err
		}
		right, err := r.rewriteCondition(v.Right)
		if err != nil {
			return nil, err
		}
		return Logical{Op: v.Op, Left: left, Right: right}, nil
	case Not:
		inner, err := r.rewriteCondition(v.Cond)
		if err != nil {
			return nil, err
		}
		return Not{Cond: inner}, nil
	case Comparison:
		col, err := r.resolveCol(v.Col, false)
		if err != nil {
			return nil, err
		}
		cmp := Comparison{Col: col, Op: v.Op, Val: v.Val}
		if v.RightCol != nil {
			right, err := r.resolveCol(*v.RightCol, false)
			if err != nil {
				return nil, err
			}
			cmp.RightCol = &right
		}
		return cmp, nil
	case In:
		col, err := r.resolveCol(v.Col, false)
		if err != nil {
			return nil, err
		}
		return In{Col: col, Vals: v.Vals}, nil
	case Like:
		col, err := r.resolveCol(v.Col, false)
		if err != nil {
			return nil, err
		}
		return Like{Col: col, Pattern: v.Pattern}, nil
	case IsNull:
		col, err := r.resolveCol(v.Col, false)
		if err != nil {
			return nil, err
		}
		return IsNull{Col: col, Negate: v.Negate}, nil
	case Between:
		col, err := r.resolveCol(v.Col, false)
		if err != nil {
			return nil, err
		}
		return Between{Col: col, Low: v.Low, High: v.High}, nil
	default:
		return nil, fmt.Errorf("internal error: unknown condition type %T", c)
	}
}

// dbCatalog resolves identifiers against a live database through a Querier.
type dbCatalog struct {
	// q runs the introspection queries.
	q Querier

	// engine selects the introspection SQL.
	engine Engine
}

// NewCatalog returns a Catalog backed by q for the given engine.
func NewCatalog(q Querier, engine Engine) Catalog {
	return dbCatalog{q: q, engine: engine}
}

// Tables returns the visible table and view names.
func (c dbCatalog) Tables(ctx context.Context) ([]string, error) {
	return ListTables(ctx, c.q, c.engine)
}

// Columns returns the named table's column names in schema order.
func (c dbCatalog) Columns(ctx context.Context, table string) ([]string, error) {
	cols, err := DescribeTable(ctx, c.q, c.engine, table)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.Name
	}
	return names, nil
}

// ListTables returns the base table and view names in the connection's current schema,
// sorted. Each engine scopes to a single namespace — Postgres to current_schema() (the head
// of the search_path), MySQL to the connected database — so a table that also exists under
// another schema cannot appear twice or merge its columns during resolution. SQLite has no
// schemas.
func ListTables(ctx context.Context, q Querier, engine Engine) ([]string, error) {
	var query string
	switch engine {
	case EnginePostgres:
		query = `SELECT table_name FROM information_schema.tables ` +
			`WHERE table_schema = current_schema() ORDER BY table_name`
	case EngineMySQL:
		query = `SELECT table_name FROM information_schema.tables ` +
			`WHERE table_schema = DATABASE() ORDER BY table_name`
	case EngineSQLite:
		query = `SELECT name FROM sqlite_master ` +
			`WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%' ORDER BY name`
	default:
		return nil, errors.New("unsupported engine")
	}

	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// DescribeTable returns the columns of table in schema order, with their declared type and
// nullability. Introspection is scoped to the connection's current schema — Postgres
// current_schema(), MySQL the connected database — so a same-named table under another
// schema does not contribute columns. The table name is always a bound parameter, never
// interpolated into the SQL.
func DescribeTable(ctx context.Context, q Querier, engine Engine, table string) ([]ColumnInfo, error) {
	if !ValidIdent(table) {
		return nil, fmt.Errorf("invalid table name %q", table)
	}
	var query string
	switch engine {
	case EnginePostgres:
		query = `SELECT column_name, data_type, is_nullable FROM information_schema.columns ` +
			`WHERE table_schema = current_schema() AND table_name = $1 ` +
			`ORDER BY ordinal_position`
	case EngineMySQL:
		query = `SELECT column_name, column_type, is_nullable FROM information_schema.columns ` +
			`WHERE table_schema = DATABASE() AND table_name = ? ORDER BY ordinal_position`
	case EngineSQLite:
		query = `SELECT name, type, "notnull" FROM pragma_table_info(?)`
	default:
		return nil, errors.New("unsupported engine")
	}

	rows, err := q.QueryContext(ctx, query, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []ColumnInfo
	for rows.Next() {
		var name, typ string
		var nullable string
		if engine == EngineSQLite {
			var notnull int
			if err := rows.Scan(&name, &typ, &notnull); err != nil {
				return nil, err
			}
			cols = append(cols, ColumnInfo{Name: name, Type: typ, Nullable: notnull == 0})
			continue
		}
		if err := rows.Scan(&name, &typ, &nullable); err != nil {
			return nil, err
		}
		cols = append(cols, ColumnInfo{Name: name, Type: typ, Nullable: strings.EqualFold(nullable, "YES")})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}
