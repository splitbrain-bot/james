package sqlq

import (
	"errors"
	"fmt"
	"strconv"
)

// aggFuncs is the closed allowlist of aggregate functions. No other function name is
// representable in the grammar, so no side-effecting function can be called.
var aggFuncs = map[string]bool{
	"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
}

// Parse lexes and parses src into a Query. A parse failure — including any construct
// outside the restricted grammar, such as a write verb, a second statement, a comment, a
// subquery, or an unknown function — is returned as an error and yields no Query. A
// reserved word for a feature the grammar does not have gives an error that names the
// feature, so the caller can rephrase instead of retrying.
func Parse(src string) (*Query, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	for _, t := range toks {
		if msg, ok := unsupported[t.text]; ok && t.kind == tokKeyword {
			return nil, errors.New(msg)
		}
	}
	p := &parser{toks: toks}
	q, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tokEOF {
		return nil, fmt.Errorf("unexpected %s after query", describe(p.cur()))
	}
	return q, nil
}

// parser is the recursive-descent parser's state: the token stream and a cursor into it.
type parser struct {
	// toks is the full token stream, terminated by tokEOF.
	toks []token

	// pos is the index of the current token.
	pos int
}

// cur returns the current token.
func (p *parser) cur() token { return p.toks[p.pos] }

// next advances past the current token and returns it.
func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

// isKeyword reports whether the current token is the given keyword.
func (p *parser) isKeyword(kw string) bool {
	t := p.cur()
	return t.kind == tokKeyword && t.text == kw
}

// acceptKeyword consumes the current token if it is the given keyword, reporting whether
// it did.
func (p *parser) acceptKeyword(kw string) bool {
	if p.isKeyword(kw) {
		p.next()
		return true
	}
	return false
}

// expectKeyword consumes the given keyword or returns an error.
func (p *parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return fmt.Errorf("expected %s, got %s", kw, describe(p.cur()))
	}
	return nil
}

// expect consumes a token of the given kind, returning it, or returns an error.
func (p *parser) expect(kind tokenKind, what string) (token, error) {
	if p.cur().kind != kind {
		return token{}, fmt.Errorf("expected %s, got %s", what, describe(p.cur()))
	}
	return p.next(), nil
}

// parseQuery parses the whole query production.
func (p *parser) parseQuery() (*Query, error) {
	if err := p.expectKeyword("SELECT"); err != nil {
		return nil, err
	}
	q := &Query{}

	if p.cur().kind == tokStar {
		p.next()
		q.Star = true
	} else {
		items, err := p.parseSelectList()
		if err != nil {
			return nil, err
		}
		q.Columns = items
	}

	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	from, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	q.From = from

	for p.atJoinStart() {
		j, err := p.parseJoin()
		if err != nil {
			return nil, err
		}
		q.Joins = append(q.Joins, j)
	}

	if p.acceptKeyword("WHERE") {
		cond, err := p.parseCondition()
		if err != nil {
			return nil, err
		}
		q.Where = cond
	}

	if p.acceptKeyword("GROUP") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		cols, err := p.parseColumnList()
		if err != nil {
			return nil, err
		}
		q.GroupBy = cols
	}

	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		items, err := p.parseOrderList()
		if err != nil {
			return nil, err
		}
		q.OrderBy = items
	}

	if p.acceptKeyword("LIMIT") {
		limit, err := p.parseNonNegInt("LIMIT")
		if err != nil {
			return nil, err
		}
		q.Limit = &limit
		if p.acceptKeyword("OFFSET") {
			offset, err := p.parseNonNegInt("OFFSET")
			if err != nil {
				return nil, err
			}
			q.Offset = &offset
		}
	}

	return q, nil
}

// parseSelectList parses one or more comma-separated select items.
func (p *parser) parseSelectList() ([]SelectItem, error) {
	var items []SelectItem
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if p.cur().kind != tokComma {
			return items, nil
		}
		p.next()
	}
}

// parseSelectItem parses a plain column or an aggregate, with an optional AS alias.
func (p *parser) parseSelectItem() (SelectItem, error) {
	var item SelectItem
	if t := p.cur(); t.kind == tokKeyword && aggFuncs[t.text] {
		agg := p.next().text
		item.Agg = agg
		if _, err := p.expect(tokLParen, "'('"); err != nil {
			return SelectItem{}, err
		}
		if p.cur().kind == tokStar {
			if agg != "COUNT" {
				return SelectItem{}, fmt.Errorf("%s(*) is not allowed; %s requires a column", agg, agg)
			}
			p.next()
			item.Star = true
		} else {
			col, err := p.parseColumn()
			if err != nil {
				return SelectItem{}, err
			}
			item.Column = col
		}
		if _, err := p.expect(tokRParen, "')'"); err != nil {
			return SelectItem{}, err
		}
	} else {
		col, err := p.parseColumn()
		if err != nil {
			return SelectItem{}, err
		}
		if p.cur().kind == tokLParen {
			return SelectItem{}, fmt.Errorf("the grammar has no function %s; only COUNT, SUM, AVG, MIN and MAX are allowed", col.Name)
		}
		item.Column = col
	}

	if p.acceptKeyword("AS") {
		alias, err := p.expect(tokIdent, "an alias identifier")
		if err != nil {
			return SelectItem{}, err
		}
		item.Alias = alias.text
	}
	return item, nil
}

// parseColumn parses "ident" or "ident.ident".
func (p *parser) parseColumn() (Column, error) {
	first, err := p.expect(tokIdent, "a column name")
	if err != nil {
		return Column{}, err
	}
	if p.cur().kind == tokDot {
		p.next()
		second, err := p.expect(tokIdent, "a column name after '.'")
		if err != nil {
			return Column{}, err
		}
		return Column{Table: first.text, Name: second.text}, nil
	}
	return Column{Name: first.text}, nil
}

// parseColumnList parses one or more comma-separated columns.
func (p *parser) parseColumnList() ([]Column, error) {
	var cols []Column
	for {
		col, err := p.parseColumn()
		if err != nil {
			return nil, err
		}
		cols = append(cols, col)
		if p.cur().kind != tokComma {
			return cols, nil
		}
		p.next()
	}
}

// parseOrderList parses one or more comma-separated order items.
func (p *parser) parseOrderList() ([]OrderItem, error) {
	var items []OrderItem
	for {
		col, err := p.parseColumn()
		if err != nil {
			return nil, err
		}
		item := OrderItem{Column: col}
		switch {
		case p.acceptKeyword("DESC"):
			item.Desc = true
		case p.acceptKeyword("ASC"):
			item.Desc = false
		}
		items = append(items, item)
		if p.cur().kind != tokComma {
			return items, nil
		}
		p.next()
	}
}

// parseTableRef parses "ident [[AS] ident]".
func (p *parser) parseTableRef() (TableRef, error) {
	name, err := p.expect(tokIdent, "a table name")
	if err != nil {
		return TableRef{}, err
	}
	ref := TableRef{Name: name.text}
	if p.acceptKeyword("AS") {
		alias, err := p.expect(tokIdent, "an alias identifier")
		if err != nil {
			return TableRef{}, err
		}
		ref.Alias = alias.text
	} else if p.cur().kind == tokIdent {
		ref.Alias = p.next().text
	}
	return ref, nil
}

// atJoinStart reports whether the current token begins a join clause.
func (p *parser) atJoinStart() bool {
	return p.isKeyword("JOIN") || p.isKeyword("INNER") || p.isKeyword("LEFT") || p.isKeyword("RIGHT")
}

// parseJoin parses one join clause.
func (p *parser) parseJoin() (Join, error) {
	var j Join
	switch {
	case p.acceptKeyword("INNER"):
		j.Type = InnerJoin
		if err := p.expectKeyword("JOIN"); err != nil {
			return Join{}, err
		}
	case p.acceptKeyword("LEFT"):
		j.Type = LeftJoin
		p.acceptKeyword("OUTER")
		if err := p.expectKeyword("JOIN"); err != nil {
			return Join{}, err
		}
	case p.acceptKeyword("RIGHT"):
		j.Type = RightJoin
		p.acceptKeyword("OUTER")
		if err := p.expectKeyword("JOIN"); err != nil {
			return Join{}, err
		}
	default:
		if err := p.expectKeyword("JOIN"); err != nil {
			return Join{}, err
		}
		j.Type = InnerJoin
	}

	ref, err := p.parseTableRef()
	if err != nil {
		return Join{}, err
	}
	j.Table = ref

	if err := p.expectKeyword("ON"); err != nil {
		return Join{}, err
	}
	cond, err := p.parseCondition()
	if err != nil {
		return Join{}, err
	}
	j.On = cond
	return j, nil
}

// parseCondition parses a full boolean condition (the or_term production).
func (p *parser) parseCondition() (Condition, error) {
	left, err := p.parseAndTerm()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		right, err := p.parseAndTerm()
		if err != nil {
			return nil, err
		}
		left = Logical{Op: OpOr, Left: left, Right: right}
	}
	return left, nil
}

// parseAndTerm parses a sequence of not-factors joined by AND.
func (p *parser) parseAndTerm() (Condition, error) {
	left, err := p.parseNotFactor()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		right, err := p.parseNotFactor()
		if err != nil {
			return nil, err
		}
		left = Logical{Op: OpAnd, Left: left, Right: right}
	}
	return left, nil
}

// parseNotFactor parses an optional NOT before a factor.
func (p *parser) parseNotFactor() (Condition, error) {
	if p.acceptKeyword("NOT") {
		inner, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return Not{Cond: inner}, nil
	}
	return p.parseFactor()
}

// parseFactor parses a parenthesized condition or a single predicate.
func (p *parser) parseFactor() (Condition, error) {
	if p.cur().kind == tokLParen {
		p.next()
		cond, err := p.parseCondition()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen, "')'"); err != nil {
			return nil, err
		}
		return cond, nil
	}
	return p.parsePredicate()
}

// parsePredicate parses one predicate: a comparison, IN, LIKE, IS [NOT] NULL, or BETWEEN.
func (p *parser) parsePredicate() (Condition, error) {
	col, err := p.parseColumn()
	if err != nil {
		return nil, err
	}

	switch {
	case p.cur().kind == tokOp:
		op := p.next().text
		// The right-hand side is a column (an identifier) or a literal value.
		if p.cur().kind == tokIdent {
			right, err := p.parseColumn()
			if err != nil {
				return nil, err
			}
			return Comparison{Col: col, Op: op, RightCol: &right}, nil
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return Comparison{Col: col, Op: op, Val: val}, nil

	case p.acceptKeyword("IN"):
		if _, err := p.expect(tokLParen, "'('"); err != nil {
			return nil, err
		}
		var vals []Value
		for {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
			if p.cur().kind != tokComma {
				break
			}
			p.next()
		}
		if _, err := p.expect(tokRParen, "')'"); err != nil {
			return nil, err
		}
		return In{Col: col, Vals: vals}, nil

	case p.acceptKeyword("LIKE"):
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		if v.Kind != StringVal {
			return nil, errors.New("LIKE requires a string pattern")
		}
		return Like{Col: col, Pattern: v}, nil

	case p.acceptKeyword("IS"):
		negate := p.acceptKeyword("NOT")
		if err := p.expectKeyword("NULL"); err != nil {
			return nil, err
		}
		return IsNull{Col: col, Negate: negate}, nil

	case p.acceptKeyword("BETWEEN"):
		low, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		high, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return Between{Col: col, Low: low, High: high}, nil

	default:
		return nil, fmt.Errorf("expected a comparison operator, IN, LIKE, IS, or BETWEEN, got %s", describe(p.cur()))
	}
}

// parseValue parses a literal: string, number, boolean, or NULL.
func (p *parser) parseValue() (Value, error) {
	t := p.cur()
	switch {
	case t.kind == tokString:
		p.next()
		return Value{Kind: StringVal, Go: t.text}, nil
	case t.kind == tokNumber:
		p.next()
		g, err := parseNumber(t.text)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: NumberVal, Go: g}, nil
	case p.isKeyword("TRUE"):
		p.next()
		return Value{Kind: BoolVal, Go: true}, nil
	case p.isKeyword("FALSE"):
		p.next()
		return Value{Kind: BoolVal, Go: false}, nil
	case p.isKeyword("NULL"):
		p.next()
		return Value{Kind: NullVal, Go: nil}, nil
	default:
		return Value{}, fmt.Errorf("expected a value, got %s", describe(t))
	}
}

// parseNonNegInt parses a non-negative integer literal for a LIMIT or OFFSET clause.
func (p *parser) parseNonNegInt(clause string) (int, error) {
	t := p.cur()
	if t.kind != tokNumber {
		return 0, fmt.Errorf("%s requires an integer, got %s", clause, describe(t))
	}
	p.next()
	v, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%s requires a non-negative integer, got %q", clause, t.text)
	}
	return int(v), nil
}

// describe renders a token for an error message.
func describe(t token) string {
	switch t.kind {
	case tokEOF:
		return "end of input"
	case tokIdent:
		return fmt.Sprintf("identifier %q", t.text)
	case tokKeyword:
		return t.text
	case tokString:
		return "string literal"
	case tokNumber:
		return fmt.Sprintf("number %q", t.text)
	default:
		return fmt.Sprintf("%q", t.text)
	}
}
