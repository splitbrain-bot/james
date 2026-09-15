package sqlq

import (
	"errors"
	"fmt"

	"github.com/doug-martin/goqu/v9"
	// The dialect packages register themselves on import; each supplies the identifier
	// quoting and placeholder style Translate emits for its engine.
	_ "github.com/doug-martin/goqu/v9/dialect/mysql"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlite3"
	"github.com/doug-martin/goqu/v9/exp"
)

// Translate turns a resolved Query into a prepared SQL statement and its bound arguments
// for the engine's dialect. Every identifier is emitted as a dialect-quoted identifier and
// every client value as a bound parameter: no client token reaches the SQL text. The
// Prepared(true) call is load-bearing — without it the builder interpolates literals into
// the SQL string instead of binding them. Translate checks every identifier itself, because
// the builder does not escape quotes inside identifiers, and it refuses a limit below one,
// because the builder drops a LIMIT of zero instead of emitting it.
func Translate(q *Query, engine Engine) (string, []any, error) {
	if err := checkIdents(q); err != nil {
		return "", nil, err
	}
	if q.Limit != nil && *q.Limit < 1 {
		return "", nil, errors.New("LIMIT must be at least 1")
	}
	ds := goqu.Dialect(engine.dialect()).From(tableExpr(q.From))

	for _, j := range q.Joins {
		on, err := buildCond(j.On, false)
		if err != nil {
			return "", nil, err
		}
		cond := goqu.On(on)
		table := tableExpr(j.Table)
		switch j.Type {
		case InnerJoin:
			ds = ds.InnerJoin(table, cond)
		case LeftJoin:
			ds = ds.LeftJoin(table, cond)
		case RightJoin:
			ds = ds.RightJoin(table, cond)
		default:
			return "", nil, errors.New("internal error: unknown join type")
		}
	}

	if !q.Star {
		sel := make([]any, len(q.Columns))
		for i, item := range q.Columns {
			sel[i] = selectExpr(item)
		}
		ds = ds.Select(sel...)
	}

	if q.Where != nil {
		where, err := buildCond(q.Where, false)
		if err != nil {
			return "", nil, err
		}
		ds = ds.Where(where)
	}

	if len(q.GroupBy) > 0 {
		gb := make([]any, len(q.GroupBy))
		for i, c := range q.GroupBy {
			gb[i] = colIdent(c)
		}
		ds = ds.GroupBy(gb...)
	}

	if len(q.OrderBy) > 0 {
		ob := make([]exp.OrderedExpression, len(q.OrderBy))
		for i, o := range q.OrderBy {
			id := colIdent(o.Column)
			if o.Desc {
				ob[i] = id.Desc()
			} else {
				ob[i] = id.Asc()
			}
		}
		ds = ds.Order(ob...)
	}

	if q.Limit != nil {
		ds = ds.Limit(uint(*q.Limit))
		if q.Offset != nil {
			ds = ds.Offset(uint(*q.Offset))
		}
	}

	sql, args, err := ds.Prepared(true).ToSQL()
	if err != nil {
		return "", nil, err
	}
	return sql, args, nil
}

// checkIdents returns an error for the first table name, alias or column name in q that is
// not a plain identifier.
func checkIdents(q *Query) error {
	var bad string
	check := func(names ...string) {
		for _, n := range names {
			if n != "" && !ValidIdent(n) && bad == "" {
				bad = n
			}
		}
	}
	checkCol := func(c Column) { check(c.Table, c.Name) }
	check(q.From.Name, q.From.Alias)
	for _, j := range q.Joins {
		check(j.Table.Name, j.Table.Alias)
		walkCondition(j.On, checkCol)
	}
	for _, item := range q.Columns {
		check(item.Alias)
		if !item.Star {
			checkCol(item.Column)
		}
	}
	if q.Where != nil {
		walkCondition(q.Where, checkCol)
	}
	for _, c := range q.GroupBy {
		checkCol(c)
	}
	for _, o := range q.OrderBy {
		checkCol(o.Column)
	}
	if bad != "" {
		return fmt.Errorf("invalid identifier %q", bad)
	}
	return nil
}

// walkCondition calls visit for every column a condition refers to.
func walkCondition(c Condition, visit func(Column)) {
	switch v := c.(type) {
	case Logical:
		walkCondition(v.Left, visit)
		walkCondition(v.Right, visit)
	case Not:
		walkCondition(v.Cond, visit)
	case Comparison:
		visit(v.Col)
		if v.RightCol != nil {
			visit(*v.RightCol)
		}
	case In:
		visit(v.Col)
	case Like:
		visit(v.Col)
	case IsNull:
		visit(v.Col)
	case Between:
		visit(v.Col)
	}
}

// tableExpr builds the identifier expression for a table reference, applying its alias.
func tableExpr(ref TableRef) exp.Expression {
	t := goqu.T(ref.Name)
	if ref.Alias != "" {
		return t.As(ref.Alias)
	}
	return t
}

// colIdent builds the identifier expression for a column. A qualified column becomes
// "table"."name"; an unqualified column (a select-list alias referenced from ORDER BY or
// GROUP BY) becomes a bare quoted identifier.
func colIdent(c Column) exp.IdentifierExpression {
	if c.Table == "" {
		return goqu.C(c.Name)
	}
	return goqu.T(c.Table).Col(c.Name)
}

// aliasable is implemented by the expression types Translate may alias — identifiers and
// aggregate function calls — letting an AS alias be applied without a type switch.
type aliasable interface {
	As(any) exp.AliasedExpression
}

// selectExpr builds the expression for one select item, applying its aggregate and alias.
// Both expression kinds it builds can carry an alias, so the assertion cannot fail.
func selectExpr(item SelectItem) any {
	var base exp.Expression
	if item.Agg != "" {
		var arg any
		if item.Star {
			arg = goqu.Star()
		} else {
			arg = colIdent(item.Column)
		}
		base = aggExpr(item.Agg, arg)
	} else {
		base = colIdent(item.Column)
	}
	if withAlias, ok := base.(aliasable); ok && item.Alias != "" {
		return withAlias.As(item.Alias)
	}
	return base
}

// aggExpr builds the aggregate function call for one of the allowlisted aggregates.
func aggExpr(agg string, arg any) exp.SQLFunctionExpression {
	switch agg {
	case "COUNT":
		return goqu.COUNT(arg)
	case "SUM":
		return goqu.SUM(arg)
	case "AVG":
		return goqu.AVG(arg)
	case "MIN":
		return goqu.MIN(arg)
	case "MAX":
		return goqu.MAX(arg)
	default:
		// Unreachable: the parser only produces the allowlisted aggregates.
		return goqu.COUNT(arg)
	}
}

// buildCond translates a condition into a goqu expression. The negate flag is pushed down
// through the tree by De Morgan's laws — AND becomes OR and vice versa, and each predicate
// switches to its negated form — because the builder has no generic NOT wrapper. This keeps
// every value bound with no literal SQL.
func buildCond(c Condition, negate bool) (exp.Expression, error) {
	switch v := c.(type) {
	case Logical:
		left, err := buildCond(v.Left, negate)
		if err != nil {
			return nil, err
		}
		right, err := buildCond(v.Right, negate)
		if err != nil {
			return nil, err
		}
		// AND under negation becomes OR, and OR under negation becomes AND.
		if (v.Op == OpAnd) != negate {
			return goqu.And(left, right), nil
		}
		return goqu.Or(left, right), nil
	case Not:
		return buildCond(v.Cond, !negate)
	case Comparison:
		return compareExpr(v, negate), nil
	case In:
		id := colIdent(v.Col)
		vals := goValues(v.Vals)
		if negate {
			return id.NotIn(vals...), nil
		}
		return id.In(vals...), nil
	case Like:
		id := colIdent(v.Col)
		if negate {
			return id.NotLike(v.Pattern.Go), nil
		}
		return id.Like(v.Pattern.Go), nil
	case IsNull:
		id := colIdent(v.Col)
		if v.Negate != negate {
			return id.IsNotNull(), nil
		}
		return id.IsNull(), nil
	case Between:
		id := colIdent(v.Col)
		r := goqu.Range(v.Low.Go, v.High.Go)
		if negate {
			return id.NotBetween(r), nil
		}
		return id.Between(r), nil
	default:
		return nil, fmt.Errorf("internal error: unknown condition type %T", c)
	}
}

// compareExpr translates a comparison, flipping the operator to its opposite under negation.
func compareExpr(v Comparison, negate bool) exp.Expression {
	op := v.Op
	if negate {
		op = negateOp(op)
	}
	id := colIdent(v.Col)
	// The right-hand operand is another column identifier or a bound value.
	var val any
	if v.RightCol != nil {
		val = colIdent(*v.RightCol)
	} else {
		val = v.Val.Go
	}
	switch op {
	case "=":
		return id.Eq(val)
	case "!=", "<>":
		return id.Neq(val)
	case "<":
		return id.Lt(val)
	case "<=":
		return id.Lte(val)
	case ">":
		return id.Gt(val)
	case ">=":
		return id.Gte(val)
	default:
		// Unreachable: the parser only produces the operators above.
		return id.Eq(val)
	}
}

// negateOp returns the logical opposite of a comparison operator.
func negateOp(op string) string {
	switch op {
	case "=":
		return "!="
	case "!=", "<>":
		return "="
	case "<":
		return ">="
	case "<=":
		return ">"
	case ">":
		return "<="
	case ">=":
		return "<"
	default:
		return op
	}
}

// goValues extracts the bound Go values from a list of literals.
func goValues(vals []Value) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v.Go
	}
	return out
}
