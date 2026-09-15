package tools

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"james/internal/agent"
	"james/internal/config"
	"james/internal/sqlq"

	// The drivers register themselves as "mysql", "pgx" and "sqlite".
	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

// dbGrammar is the description of the query language shown to the model.
//
//go:embed db_prompt.md
var dbGrammar string

const (
	// dbMaxOpenConns caps the open connections of one pool.
	dbMaxOpenConns = 4
	// dbMaxIdleConns caps the idle connections kept in one pool.
	dbMaxIdleConns = 2
	// dbConnMaxIdleTime is how long an idle connection is kept.
	dbConnMaxIdleTime = 5 * time.Minute
	// dbDefaultMaxRows is the row cap used when the configuration names none.
	dbDefaultMaxRows = 200
	// dbDefaultTimeout is the query timeout used when the configuration names
	// none.
	dbDefaultTimeout = 10 * time.Second
	// dbMaxOutput caps the text one database tool returns.
	dbMaxOutput = 50 * 1024
)

// dbCellReplacer folds the characters that would break a row into one line.
var dbCellReplacer = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")

// dbConn is one open database connection with its limits.
type dbConn struct {
	// db is the pooled handle.
	db *sql.DB
	// engine selects the query dialect and the schema queries.
	engine sqlq.Engine
	// maxRows caps the rows one query returns.
	maxRows int
	// timeout caps the run time of one query.
	timeout time.Duration
}

// begin starts a read-only transaction as a second barrier against writes. The
// caller has to roll it back.
func (c *dbConn) begin(ctx context.Context) (*sql.Tx, error) {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	if c.engine == sqlq.EnginePostgres {
		// SET LOCAL keeps the timeout inside this transaction, off the pooled
		// connection. Postgres binds no parameter here, and the value comes
		// from the configuration.
		stmt := fmt.Sprintf("SET LOCAL statement_timeout = %d", c.timeout.Milliseconds())
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}

// dbSet holds the open connections of all configured databases.
type dbSet struct {
	// conns maps a connection name to its connection.
	conns map[string]*dbConn
	// names lists the connection names in sorted order.
	names []string
}

// lookup returns the named connection. An empty name picks the only connection
// when just one is configured.
func (s *dbSet) lookup(name string) (*dbConn, error) {
	if name == "" {
		if len(s.names) == 1 {
			return s.conns[s.names[0]], nil
		}
		return nil, fmt.Errorf("no connection given, pick one of: %s", strings.Join(s.names, ", "))
	}
	conn, ok := s.conns[name]
	if !ok {
		return nil, fmt.Errorf("unknown connection %q, pick one of: %s", name, strings.Join(s.names, ", "))
	}
	return conn, nil
}

// Close closes every open connection and returns the first error. Closing a
// set twice is allowed and does nothing the second time.
func (s *dbSet) Close() error {
	var first error
	for _, c := range s.conns {
		if err := c.db.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// list returns one line per connection, naming it and its engine.
func (s *dbSet) list() string {
	var b strings.Builder
	for _, n := range s.names {
		fmt.Fprintf(&b, "  %s (%s)\n", n, s.conns[n].engine)
	}
	return b.String()
}

// NewDBTools opens one connection per configured database and returns the
// db_schema and db_query tools for them. It pings every connection, so a wrong
// setting fails at start. The returned closer closes the connections; it is
// usable even when NewDBTools fails, which closes what was already open.
func NewDBTools(ctx context.Context, dbs map[string]config.Database) ([]agent.Tool, io.Closer, error) {
	set := &dbSet{conns: make(map[string]*dbConn, len(dbs))}
	if len(dbs) == 0 {
		return nil, set, nil
	}

	// The databases are opened in name order, so the same wrong setting always
	// reports the same first failure.
	for _, name := range slices.Sorted(maps.Keys(dbs)) {
		conn, err := dbOpen(dbs[name])
		if err != nil {
			_ = set.Close()
			return nil, set, fmt.Errorf("database %q: %w", name, err)
		}
		set.conns[name] = conn
		set.names = append(set.names, name)
	}

	for _, name := range set.names {
		conn := set.conns[name]
		pingCtx, cancel := context.WithTimeout(ctx, conn.timeout)
		err := conn.db.PingContext(pingCtx)
		cancel()
		if err != nil {
			_ = set.Close()
			return nil, set, fmt.Errorf("database %q: %w", name, err)
		}
	}

	return []agent.Tool{newDBSchemaTool(set), newDBQueryTool(set)}, set, nil
}

// dbOpen opens the pool for one configured database.
func dbOpen(cfg config.Database) (*dbConn, error) {
	var engine sqlq.Engine
	var driver, dsn string
	switch cfg.Driver {
	case "mysql":
		hardened, err := dbMySQLDSN(cfg.DSN)
		if err != nil {
			return nil, err
		}
		engine, driver, dsn = sqlq.EngineMySQL, "mysql", hardened
	case "postgres":
		engine, driver, dsn = sqlq.EnginePostgres, "pgx", cfg.DSN
	case "sqlite":
		readOnly, err := dbSQLiteDSN(cfg.DSN)
		if err != nil {
			return nil, err
		}
		engine, driver, dsn = sqlq.EngineSQLite, "sqlite", readOnly
	default:
		return nil, fmt.Errorf("unknown driver %q, use mysql, postgres or sqlite", cfg.Driver)
	}

	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(dbMaxOpenConns)
	db.SetMaxIdleConns(dbMaxIdleConns)
	db.SetConnMaxIdleTime(dbConnMaxIdleTime)

	conn := &dbConn{db: db, engine: engine, maxRows: cfg.MaxRows, timeout: cfg.Timeout}
	if conn.maxRows <= 0 {
		conn.maxRows = dbDefaultMaxRows
	}
	if conn.timeout <= 0 {
		conn.timeout = dbDefaultTimeout
	}
	return conn, nil
}

// dbMySQLDSN returns the DSN with the driver options MultiStatements and
// AllowAllFiles off, so one call carries one statement and the server cannot
// read local files.
func dbMySQLDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	cfg.MultiStatements = false
	cfg.AllowAllFiles = false
	return cfg.FormatDSN(), nil
}

// dbSQLiteDSN checks that the database file exists and returns the DSN that
// opens it with the query_only pragma, so the engine itself refuses writes. The
// check comes first because the driver creates a missing file instead of failing.
func dbSQLiteDSN(dsn string) (string, error) {
	path := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a database file", path)
	}

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return "file:" + strings.TrimPrefix(dsn, "file:") + sep + "_pragma=query_only(1)", nil
}

// dbInput is the input of both database tools.
type dbInput struct {
	// Connection is the name of the database to use.
	Connection string `json:"connection"`
	// Table is the single table db_schema describes.
	Table string `json:"table"`
	// Query is the query db_query runs.
	Query string `json:"query"`
}

// dbSchemaProps is the JSON Schema of the db_schema input properties.
const dbSchemaProps = `"connection":{"type":"string","description":"Name of the database connection."},` +
	`"table":{"type":"string","description":"Table to describe. Leave it out to list all tables."}`

// dbQueryProps is the JSON Schema of the db_query input properties.
const dbQueryProps = `"connection":{"type":"string","description":"Name of the database connection."},` +
	`"query":{"type":"string","description":"The query in the grammar of this tool."}`

// dbInputSchema builds the JSON Schema of a tool input from its properties. It
// requires the connection when more than one is configured.
func dbInputSchema(set *dbSet, props string, required ...string) json.RawMessage {
	if len(set.names) > 1 {
		required = append(required, "connection")
	}
	schema := `{"type":"object","properties":{` + props + `}`
	if len(required) > 0 {
		list, _ := json.Marshal(required)
		schema += `,"required":` + string(list)
	}
	return json.RawMessage(schema + `}`)
}

// dbSchemaTool reports the tables and columns of one connection.
type dbSchemaTool struct {
	// set holds the connections it may read.
	set *dbSet
	// description is the text shown to the model.
	description string
	// schema is the JSON Schema of the input.
	schema json.RawMessage
}

// newDBSchemaTool builds the db_schema tool for the given connections.
func newDBSchemaTool(set *dbSet) *dbSchemaTool {
	desc := "Shows the schema of a configured database. Without a table name it lists every " +
		"table with its columns and their types. With a table name it shows that table's " +
		"columns, types and whether they take NULL.\n\nConnections:\n" + set.list()
	if len(set.names) == 1 {
		desc += "\nThe connection may be left out."
	}
	return &dbSchemaTool{set: set, description: desc, schema: dbInputSchema(set, dbSchemaProps)}
}

// Name returns the tool name.
func (t *dbSchemaTool) Name() string { return "db_schema" }

// Description returns the text shown to the model.
func (t *dbSchemaTool) Description() string { return t.description }

// Schema returns the JSON Schema of the tool input.
func (t *dbSchemaTool) Schema() json.RawMessage { return t.schema }

// Run lists the tables of a connection, or describes one table of it.
func (t *dbSchemaTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in dbInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agent.Result{}, err
	}
	conn, err := t.set.lookup(in.Connection)
	if err != nil {
		return agent.Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, conn.timeout)
	defer cancel()
	tx, err := conn.begin(ctx)
	if err != nil {
		return agent.Result{}, err
	}
	defer tx.Rollback()

	var b strings.Builder
	if in.Table != "" {
		if !sqlq.ValidIdent(in.Table) {
			return agent.Result{}, fmt.Errorf("invalid table name %q", in.Table)
		}
		cols, err := sqlq.DescribeTable(ctx, tx, conn.engine, in.Table)
		if err != nil {
			return agent.Result{}, err
		}
		if len(cols) == 0 {
			return agent.Result{}, fmt.Errorf("unknown table %q", in.Table)
		}
		dbWriteTable(&b, in.Table, cols, true)
	} else {
		tables, err := sqlq.ListTables(ctx, tx, conn.engine)
		if err != nil {
			return agent.Result{}, err
		}
		if len(tables) == 0 {
			return agent.Result{Text: "The database has no tables."}, nil
		}
		for _, name := range tables {
			cols, err := sqlq.DescribeTable(ctx, tx, conn.engine, name)
			if err != nil {
				return agent.Result{}, err
			}
			dbWriteTable(&b, name, cols, false)
		}
	}

	text, cut := capText(strings.TrimRight(b.String(), "\n")+"\n", dbMaxOutput)
	if cut {
		text += "\nThe schema was cut off here because it is too long. Ask for single tables."
	}
	return agent.Result{Text: text}, nil
}

// dbWriteTable writes one table as its name and one indented line per column.
// With withNull each line also says whether the column takes NULL.
func dbWriteTable(b *strings.Builder, table string, cols []sqlq.ColumnInfo, withNull bool) {
	fmt.Fprintf(b, "%s\n", table)
	width := 0
	for _, c := range cols {
		if n := utf8.RuneCountInString(c.Name); n > width {
			width = n
		}
	}
	for _, c := range cols {
		fmt.Fprintf(b, "  %-*s %s", width, c.Name, c.Type)
		if withNull {
			if c.Nullable {
				b.WriteString(" NULL")
			} else {
				b.WriteString(" NOT NULL")
			}
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
}

// dbQueryTool runs one query of the read-only grammar against a connection.
type dbQueryTool struct {
	// set holds the connections it may query.
	set *dbSet
	// description is the text shown to the model.
	description string
	// schema is the JSON Schema of the input.
	schema json.RawMessage
}

// newDBQueryTool builds the db_query tool for the given connections.
func newDBQueryTool(set *dbSet) *dbQueryTool {
	desc := dbGrammar + "\nConnections:\n" + set.list()
	if len(set.names) == 1 {
		desc += "\nThe connection may be left out."
	}
	return &dbQueryTool{set: set, description: desc, schema: dbInputSchema(set, dbQueryProps, "query")}
}

// Name returns the tool name.
func (t *dbQueryTool) Name() string { return "db_query" }

// Description returns the grammar and the connections the model may query.
func (t *dbQueryTool) Description() string { return t.description }

// Schema returns the JSON Schema of the tool input.
func (t *dbQueryTool) Schema() json.RawMessage { return t.schema }

// Run parses the query, resolves its names against the live schema and returns
// the rows as a text table.
func (t *dbQueryTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in dbInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agent.Result{}, err
	}
	conn, err := t.set.lookup(in.Connection)
	if err != nil {
		return agent.Result{}, err
	}

	q, err := sqlq.Parse(in.Query)
	if err != nil {
		return agent.Result{}, fmt.Errorf("the query does not fit the grammar: %w. "+
			"The tool description lists what the grammar has", err)
	}
	limit, imposed := dbApplyLimit(q, conn.maxRows)

	ctx, cancel := context.WithTimeout(ctx, conn.timeout)
	defer cancel()
	tx, err := conn.begin(ctx)
	if err != nil {
		return agent.Result{}, err
	}
	defer tx.Rollback()

	if err := sqlq.Resolve(ctx, sqlq.NewCatalog(tx, conn.engine), q); err != nil {
		return agent.Result{}, err
	}
	stmt, args, err := sqlq.Translate(q, conn.engine)
	if err != nil {
		return agent.Result{}, err
	}

	rows, err := tx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return agent.Result{}, err
	}
	defer rows.Close()
	cols, data, err := dbScanRows(rows)
	if err != nil {
		return agent.Result{}, err
	}
	if len(data) == 0 {
		return agent.Result{Text: "The query returned no rows."}, nil
	}

	text, cut := capText(dbRenderRows(cols, data), dbMaxOutput)
	var notes []string
	if imposed && len(data) >= limit {
		notes = append(notes, fmt.Sprintf("Cut off at the row limit of %d rows. "+
			"There may be more rows; use LIMIT with OFFSET to read on.", limit))
	}
	if cut {
		notes = append(notes, "Cut off at 50 KB of text. Ask for fewer columns or fewer rows.")
	}
	if len(notes) > 0 {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += strings.Join(notes, "\n") + "\n"
	}
	return agent.Result{Text: text}, nil
}

// dbApplyLimit sets the row limit of the query and reports it. The imposed flag
// says the limit comes from the configuration, so rows may be missing. A
// missing limit, a limit above the cap and a limit of zero all become the cap,
// because a limit of zero would remove the LIMIT clause.
func dbApplyLimit(q *sqlq.Query, maxRows int) (limit int, imposed bool) {
	if q.Limit == nil || *q.Limit > maxRows || *q.Limit <= 0 {
		q.Limit = &maxRows
		return maxRows, true
	}
	return *q.Limit, false
}

// dbScanRows reads all rows as text, with their column names.
func dbScanRows(rs *sql.Rows) (cols []string, rows [][]string, err error) {
	cols, err = rs.Columns()
	if err != nil {
		return nil, nil, err
	}
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = dbCell(v)
		}
		rows = append(rows, row)
	}
	if err := rs.Err(); err != nil {
		return nil, nil, err
	}
	return cols, rows, nil
}

// dbCell renders one value as a single line of text. Tabs and line breaks
// become spaces so that one row stays one line. Bytes that are not text are
// shown in hex.
func dbCell(v any) string {
	var s string
	switch t := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		if !utf8.Valid(t) {
			return fmt.Sprintf("0x%x", t)
		}
		s = string(t)
	case string:
		s = t
	case time.Time:
		s = t.Format(time.RFC3339)
	default:
		s = fmt.Sprint(t)
	}
	return dbCellReplacer.Replace(s)
}

// dbRenderRows renders the rows as lines of tab separated cells, under a header
// line of column names.
func dbRenderRows(cols []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString(strings.Join(cols, "\t"))
	b.WriteByte('\n')
	for _, r := range rows {
		b.WriteString(strings.Join(r, "\t"))
		b.WriteByte('\n')
	}
	return b.String()
}

// capText cuts text to at most max bytes, ending on a whole character, and
// reports whether it had to cut.
func capText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
