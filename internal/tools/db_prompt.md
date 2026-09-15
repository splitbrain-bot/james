Runs one read-only query against a configured database and returns the rows as a
text table. The query language looks like SQL, but it is a small grammar of its
own that can only read. A write, a subquery or an unknown function is a syntax
error, not a denied operation.

Shape of a query:

    SELECT * | item [, item ...]
    FROM table [alias]
    [ [INNER | LEFT [OUTER] | RIGHT [OUTER]] JOIN table [alias] ON condition ]
    [WHERE condition]
    [GROUP BY column [, column ...]]
    [ORDER BY column [ASC | DESC] [, ...]]
    [LIMIT n [OFFSET n]]

    item       column, COUNT(*), or COUNT|SUM|AVG|MIN|MAX(column).
               Each item may carry "AS name". Only COUNT takes a star.
    column     name, or table.name where table is the table or its alias
    condition  predicates joined by AND, OR and NOT, grouped by parentheses
    predicate  column = | <> | != | < | <= | > | >= value or other column
               column IN (value [, value ...])
               column LIKE 'pattern'
               column IS [NOT] NULL
               column BETWEEN value AND value
    value      'text' in single quotes (write '' for a quote inside), a number
               like 42 or 1.5, TRUE, FALSE, or NULL

Keywords may be written in any case. A table alias may stand right after the
table name, a column alias always needs AS. GROUP BY and ORDER BY may use a
column alias from the select list. Values are always bound as parameters.

Not supported: subqueries, UNION, HAVING, DISTINCT, computed expressions such as
`a + b`, `CONCAT(...)` or `CASE`, any function beyond the five aggregates,
comments, and more than one statement per call. Split a question that needs one
of these into simpler queries and combine the results yourself.

Without LIMIT a server row limit applies. The answer says when it cut the rows or
the text. Call db_schema first to learn the table and column names.
