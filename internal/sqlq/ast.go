package sqlq

// Query is a parsed read-only query: the root of the AST the parser produces and the
// translator consumes. It mirrors the restricted grammar exactly — a SELECT list, a FROM
// table with optional JOINs, and optional WHERE, GROUP BY, ORDER BY, and LIMIT clauses.
// The grammar has no write production, so a value of this type can only ever describe a
// SELECT.
type Query struct {
	// Star is true for "SELECT *"; Columns is then empty.
	Star bool

	// Columns is the explicit select list when Star is false.
	Columns []SelectItem

	// From is the primary table.
	From TableRef

	// Joins are the joined tables, applied left to right after From.
	Joins []Join

	// Where is the WHERE condition, or nil when the clause is absent.
	Where Condition

	// GroupBy lists the GROUP BY columns, empty when the clause is absent.
	GroupBy []Column

	// OrderBy lists the ORDER BY items, empty when the clause is absent.
	OrderBy []OrderItem

	// Limit is the row limit, or nil when the client gave none (the caller applies a
	// server default).
	Limit *int

	// Offset is the row offset, or nil when the clause is absent.
	Offset *int
}

// SelectItem is one entry in the select list: a plain column or an aggregate over a column
// or "*", with an optional output alias.
type SelectItem struct {
	// Agg is the aggregate function name in upper case (COUNT, SUM, AVG, MIN, MAX), or ""
	// for a plain column.
	Agg string

	// Star is true for an aggregate over "*" (only COUNT(*)); Column is then unused.
	Star bool

	// Column is the plain column, or the aggregate's argument when Star is false.
	Column Column

	// Alias is the AS output name, or "" when none was given.
	Alias string
}

// Column is a column reference: an optional table qualifier and a name. After catalog
// resolution a real column always carries a qualifier (Table set to the binding it belongs
// to); a bare Name with no Table denotes a select-list alias referenced from ORDER BY or
// GROUP BY.
type Column struct {
	// Table is the qualifier before the dot, or "" when unqualified.
	Table string

	// Name is the column name, or an alias name for an alias reference.
	Name string
}

// TableRef is a table named in FROM or JOIN, with an optional alias.
type TableRef struct {
	// Name is the table name.
	Name string

	// Alias is the local alias, or "" when none was given.
	Alias string
}

// JoinType is the kind of a JOIN.
type JoinType int

const (
	// InnerJoin is [INNER] JOIN.
	InnerJoin JoinType = iota
	// LeftJoin is LEFT [OUTER] JOIN.
	LeftJoin
	// RightJoin is RIGHT [OUTER] JOIN.
	RightJoin
)

// Join is a joined table and its ON condition.
type Join struct {
	// Type is the join kind.
	Type JoinType

	// Table is the joined table.
	Table TableRef

	// On is the join condition.
	On Condition
}

// OrderItem is one ORDER BY entry: a column and its direction.
type OrderItem struct {
	// Column is the ordering column (or a select-list alias).
	Column Column

	// Desc is true for DESC, false for the default ASC.
	Desc bool
}

// Condition is a boolean expression in a WHERE or ON clause. The set of concrete
// implementations is closed — logical combinations and the fixed predicate forms below —
// so no free expression, arbitrary function, or subquery is representable.
type Condition interface {
	isCondition()
}

// LogicalOp is the operator of a Logical condition.
type LogicalOp int

const (
	// OpAnd is the AND operator.
	OpAnd LogicalOp = iota
	// OpOr is the OR operator.
	OpOr
)

// Logical is a binary AND or OR of two conditions.
type Logical struct {
	// Op is AND or OR.
	Op LogicalOp

	// Left and Right are the operands.
	Left, Right Condition
}

// Not is a logical negation of a condition.
type Not struct {
	// Cond is the negated condition.
	Cond Condition
}

// Comparison is "column op value" or "column op column" with a fixed comparison operator.
// A column on the right lets JOIN conditions and same-row column comparisons be expressed;
// both sides are resolved identifiers quoted by the translator, never client text.
type Comparison struct {
	// Col is the left-hand column.
	Col Column

	// Op is one of =, !=, <>, <, <=, >, >=.
	Op string

	// Val is the right-hand bound value when RightCol is nil.
	Val Value

	// RightCol, when non-nil, is a column compared against Col in place of a value.
	RightCol *Column
}

// In is "column IN (value, ...)".
type In struct {
	// Col is the tested column.
	Col Column

	// Vals is the non-empty list of candidate values.
	Vals []Value
}

// Like is "column LIKE 'pattern'".
type Like struct {
	// Col is the tested column.
	Col Column

	// Pattern is the string pattern value.
	Pattern Value
}

// IsNull is "column IS NULL" or "column IS NOT NULL".
type IsNull struct {
	// Col is the tested column.
	Col Column

	// Negate is true for IS NOT NULL.
	Negate bool
}

// Between is "column BETWEEN low AND high".
type Between struct {
	// Col is the tested column.
	Col Column

	// Low and High are the inclusive bound values.
	Low, High Value
}

func (Logical) isCondition()    {}
func (Not) isCondition()        {}
func (Comparison) isCondition() {}
func (In) isCondition()         {}
func (Like) isCondition()       {}
func (IsNull) isCondition()     {}
func (Between) isCondition()    {}

// ValueKind is the type of a literal Value.
type ValueKind int

const (
	// StringVal is a single-quoted string literal.
	StringVal ValueKind = iota
	// NumberVal is a numeric literal (integer or floating point).
	NumberVal
	// BoolVal is TRUE or FALSE.
	BoolVal
	// NullVal is NULL.
	NullVal
)

// Value is a literal from the query text. It is always translated to a bound parameter,
// never interpolated into the SQL string.
type Value struct {
	// Kind is the literal's type.
	Kind ValueKind

	// Go is the bound Go value handed to the query builder: string for StringVal, int64 or
	// float64 for NumberVal, bool for BoolVal, and nil for NullVal.
	Go any
}
