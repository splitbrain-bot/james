// Package sqlq is the read-only query language: a small, SQL-looking grammar that is
// not SQL. Its front end (lexer, parser) is hand-rolled and depends only on the standard
// library; its back end translates the parsed query into a real SQL SELECT with a
// multi-dialect query builder. Read-only is a property of the grammar rather than a check:
// there is no production that emits a write, so a write is a syntax error, unrepresentable
// on any account. Client identifiers are validated and resolved against the live catalog
// and handed to the builder as quoted identifiers; client values are always bound as
// parameters. No client token ever reaches the SQL string as text.
package sqlq

// Engine identifies a database engine, selecting the query builder dialect and the
// catalog-introspection queries used to resolve identifiers.
type Engine int

const (
	// EnginePostgres is PostgreSQL, reached through the pgx driver.
	EnginePostgres Engine = iota
	// EngineMySQL is MySQL or MariaDB, reached through go-sql-driver.
	EngineMySQL
	// EngineSQLite is SQLite, reached through the pure-Go modernc driver.
	EngineSQLite
)

// String returns the engine's short name.
func (e Engine) String() string {
	switch e {
	case EnginePostgres:
		return "postgres"
	case EngineMySQL:
		return "mysql"
	case EngineSQLite:
		return "sqlite"
	default:
		return "unknown"
	}
}

// dialect returns the goqu dialect name for the engine.
func (e Engine) dialect() string {
	switch e {
	case EnginePostgres:
		return "postgres"
	case EngineMySQL:
		return "mysql"
	case EngineSQLite:
		return "sqlite3"
	default:
		return "default"
	}
}
