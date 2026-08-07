package sqlsee

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

type sqlClauseKind uint8

const (
	sqlClauseJoin sqlClauseKind = iota + 1
	sqlClauseWhere
	sqlClauseSelect
)

type sqlClause struct {
	kind  sqlClauseKind
	sql   string
	alias string
	args  []any
}

// Option configures a Query during construction.
type Option struct {
	plugin Plugin
}

// WithPlugin installs a plugin on a Query.
func WithPlugin(plugin Plugin) Option {
	return Option{plugin: plugin}
}

// Plugin adds reusable behavior to a Query. Name must be stable and unique
// within a Query. Install is called once for each Query and must return a
// handler safe for concurrent List calls.
type Plugin interface {
	Name() string
	Install(PluginContext) (PluginHandler, error)
}

// PluginHandler applies one plugin to a List call. It is invoked even when the
// call did not supply plugin input.
type PluginHandler func(context.Context, PluginInput, *PluginBuilder) error

// PluginContext exposes validated model metadata while a plugin is installed.
type PluginContext struct {
	primaryKey string
	alias      string
	fields     map[string]struct{}
}

// PrimaryKey returns the configured primary-key column name.
func (c PluginContext) PrimaryKey() string { return c.primaryKey }

// Column validates a model column and returns its quoted, alias-qualified SQL
// expression.
func (c PluginContext) Column(field string) (string, error) {
	if _, ok := c.fields[field]; !ok {
		return "", fmt.Errorf("sqlsee: unknown field %q", field)
	}

	return quote(c.alias) + "." + quote(field), nil
}

// PluginBuilder collects trusted, parameterized SQL contributed by a plugin.
// Its values are valid only for the current handler invocation.
type PluginBuilder struct {
	clauses   []sqlClause
	selectMap map[string]struct{}
}

// Join adds a complete JOIN clause after the configured table. Placeholders
// are local to the clause and start at $1.
func (b *PluginBuilder) Join(sql string, args ...any) error {
	return b.add(sqlClause{kind: sqlClauseJoin, sql: sql, args: args})
}

// Where adds a predicate combined with all other predicates using AND. The SQL
// must not include the WHERE keyword. Placeholders are local to the predicate.
func (b *PluginBuilder) Where(sql string, args ...any) error {
	return b.add(sqlClause{kind: sqlClauseWhere, sql: sql, args: args})
}

// Select adds a column to the SELECT list. The expression is appended after
// the model columns. Alias must be a non-empty, valid PostgreSQL identifier
// that does not collide with a model column or another plugin's select alias.
// Placeholders are local to the expression and start at $1.
func (b *PluginBuilder) Select(sql, alias string, args ...any) error {
	return b.add(sqlClause{kind: sqlClauseSelect, sql: sql, alias: alias, args: args})
}

func (b *PluginBuilder) add(clause sqlClause) error {
	clause.sql = strings.TrimSpace(clause.sql)
	if clause.kind == sqlClauseSelect {
		alias := strings.TrimSpace(clause.alias)
		quoted, err := QuoteIdentifier(alias)
		if err != nil {
			return fmt.Errorf("sqlsee: select alias: %w", err)
		}
		if b.selectMap == nil {
			b.selectMap = make(map[string]struct{})
		}
		if _, dup := b.selectMap[quoted]; dup {
			return fmt.Errorf("sqlsee: duplicate select alias %q", alias)
		}
		b.selectMap[quoted] = struct{}{}
		clause.alias = alias
	}
	if _, err := compileSQLClause(clause, 0); err != nil {
		return err
	}

	b.clauses = append(b.clauses, clause)
	return nil
}

// ListOption supplies typed, per-call data to an installed plugin. Plugin
// packages should expose their own helpers instead of exposing this value.
type ListOption struct {
	plugin string
	value  any
}

// NewPluginOption creates per-call data for a named plugin. It is intended for
// plugin authors; application code should use the plugin package's typed helper.
func NewPluginOption(pluginName string, value any) ListOption {
	return ListOption{plugin: pluginName, value: value}
}

// PluginInput is the per-call value supplied to a plugin handler.
type PluginInput struct {
	present bool
	value   any
}

// Present reports whether the List call supplied an option for this plugin.
func (i PluginInput) Present() bool { return i.present }

// Value returns the opaque value supplied by the plugin's ListOption helper.
func (i PluginInput) Value() any { return i.value }

type installedPlugin struct {
	name    string
	handler PluginHandler
}

type pluginResult struct {
	name    string
	clauses []sqlClause
}

type pluginFingerprint struct {
	Name    string                    `json:"name"`
	Clauses []pluginClauseFingerprint `json:"clauses,omitempty"`
}

type pluginClauseFingerprint struct {
	Kind  string `json:"kind"`
	SQL   string `json:"sql"`
	Alias string `json:"alias,omitempty"`
	Args  []any  `json:"args,omitempty"`
}

// selectColumn is a compiled, placeholder-renumbered SELECT expression with
// its alias, ready to append to the query's SELECT list.
type selectColumn struct {
	sql   string
	alias string
}

func isNilPlugin(plugin Plugin) bool {
	if plugin == nil {
		return true
	}

	value := reflect.ValueOf(plugin)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func applyPluginResults(
	b *sqlBuilder,
	results []pluginResult,
) ([]string, []selectColumn, any, error) {
	joins := []string{}
	selects := []selectColumn{}
	wheres := []string{}
	fingerprint := make([]pluginFingerprint, len(results))
	for i, result := range results {
		fingerprint[i].Name = result.name
	}

	for _, kind := range []sqlClauseKind{sqlClauseSelect, sqlClauseJoin, sqlClauseWhere} {
		for i, result := range results {
			for _, clause := range result.clauses {
				if clause.kind != kind {
					continue
				}

				compiled, err := compileSQLClause(clause, len(b.args))
				if err != nil {
					return nil, nil, nil, err
				}

				b.args = append(b.args, clause.args...)
				kindName := "where"
				switch kind {
				case sqlClauseSelect:
					kindName = "select"
					selects = append(selects, selectColumn{sql: compiled, alias: clause.alias})
				case sqlClauseJoin:
					kindName = "join"
					joins = append(joins, compiled)
				default:
					wheres = append(wheres, "("+compiled+")")
				}
				fingerprint[i].Clauses = append(fingerprint[i].Clauses, pluginClauseFingerprint{
					Kind:  kindName,
					SQL:   clause.sql,
					Alias: clause.alias,
					Args:  append([]any(nil), clause.args...),
				})
			}
		}
	}

	b.where = append(b.where, wheres...)
	if len(results) == 0 {
		return joins, selects, "", nil
	}

	return joins, selects, fingerprint, nil
}

func compileSQLClause(clause sqlClause, offset int) (string, error) {
	if clause.kind != sqlClauseJoin && clause.kind != sqlClauseWhere && clause.kind != sqlClauseSelect {
		return "", fmt.Errorf("sqlsee: invalid custom SQL clause")
	}

	sql := strings.TrimSpace(clause.sql)
	if sql == "" {
		return "", fmt.Errorf("sqlsee: custom SQL clause must not be empty")
	}

	if clause.kind == sqlClauseWhere && startsWithKeyword(sql, "WHERE") {
		return "", fmt.Errorf("sqlsee: plugin Where must not include the WHERE keyword")
	}
	if clause.kind == sqlClauseJoin && !isJoinClause(sql) {
		return "", fmt.Errorf("sqlsee: plugin Join must contain a complete JOIN clause")
	}

	used := make([]bool, len(clause.args))
	var out strings.Builder
	out.Grow(len(sql))

	for i := 0; i < len(sql); {
		switch {
		case sql[i] == '\'':
			end, err := quotedEnd(sql, i, '\'', true)
			if err != nil {
				return "", err
			}
			out.WriteString(sql[i:end])
			i = end

		case sql[i] == '"':
			end, err := quotedEnd(sql, i, '"', false)
			if err != nil {
				return "", err
			}
			out.WriteString(sql[i:end])
			i = end

		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				out.WriteString(sql[i:])
				out.WriteByte('\n')
				i = len(sql)
				continue
			}
			end += i + 1
			out.WriteString(sql[i:end])
			i = end

		case strings.HasPrefix(sql[i:], "/*"):
			end, err := blockCommentEnd(sql, i)
			if err != nil {
				return "", err
			}
			out.WriteString(sql[i:end])
			i = end

		case sql[i] == '$':
			if i > 0 && (isIdentifierPart(sql[i-1]) || sql[i-1] == '$') {
				out.WriteByte(sql[i])
				i++
				continue
			}
			if delimiter, ok := dollarQuoteDelimiter(sql, i); ok {
				start := i + len(delimiter)
				relativeEnd := strings.Index(sql[start:], delimiter)
				if relativeEnd < 0 {
					return "", fmt.Errorf("sqlsee: unterminated dollar-quoted string in custom SQL")
				}
				end := start + relativeEnd + len(delimiter)
				out.WriteString(sql[i:end])
				i = end
				continue
			}

			end := i + 1
			for end < len(sql) && sql[end] >= '0' && sql[end] <= '9' {
				end++
			}
			if end == i+1 {
				out.WriteByte(sql[i])
				i++
				continue
			}
			if sql[i+1] == '0' {
				return "", fmt.Errorf("sqlsee: custom SQL placeholders must start at $1")
			}

			position, err := strconv.Atoi(sql[i+1 : end])
			if err != nil || position < 1 || position > len(clause.args) {
				return "", fmt.Errorf("sqlsee: custom SQL placeholder %s has no argument", sql[i:end])
			}
			used[position-1] = true
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(offset + position))
			i = end

		case sql[i] == ';':
			return "", fmt.Errorf("sqlsee: custom SQL clauses must not contain semicolons")

		default:
			out.WriteByte(sql[i])
			i++
		}
	}

	for i, present := range used {
		if !present {
			return "", fmt.Errorf("sqlsee: custom SQL argument %d has no placeholder", i+1)
		}
	}

	return out.String(), nil
}

func startsWithKeyword(sql, keyword string) bool {
	if len(sql) < len(keyword) || !strings.EqualFold(sql[:len(keyword)], keyword) {
		return false
	}

	if len(sql) == len(keyword) {
		return true
	}

	next := sql[len(keyword)]
	return !isIdentifierPart(next) && next != '$'
}

func isJoinClause(sql string) bool {
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return false
	}

	for i := range fields {
		fields[i] = strings.ToUpper(fields[i])
		if i == 3 {
			break
		}
	}

	switch fields[0] {
	case "JOIN":
		return true
	case "INNER", "CROSS":
		return len(fields) > 1 && fields[1] == "JOIN"
	case "LEFT", "RIGHT", "FULL":
		return len(fields) > 1 && (fields[1] == "JOIN" ||
			fields[1] == "OUTER" && len(fields) > 2 && fields[2] == "JOIN")
	case "NATURAL":
		if len(fields) <= 1 {
			return false
		}
		if fields[1] == "JOIN" {
			return true
		}
		if fields[1] == "INNER" {
			return len(fields) > 2 && fields[2] == "JOIN"
		}
		if fields[1] == "LEFT" || fields[1] == "RIGHT" || fields[1] == "FULL" {
			return len(fields) > 2 && (fields[2] == "JOIN" ||
				fields[2] == "OUTER" && len(fields) > 3 && fields[3] == "JOIN")
		}
		return false
	default:
		return false
	}
}

func quotedEnd(sql string, start int, quote byte, backslashEscapes bool) (int, error) {
	for i := start + 1; i < len(sql); i++ {
		if backslashEscapes && sql[i] == '\\' && i+1 < len(sql) {
			i++
			continue
		}
		if sql[i] != quote {
			continue
		}
		if i+1 < len(sql) && sql[i+1] == quote {
			i++
			continue
		}

		return i + 1, nil
	}

	return 0, fmt.Errorf("sqlsee: unterminated quoted value in custom SQL")
}

func blockCommentEnd(sql string, start int) (int, error) {
	depth := 1
	for i := start + 2; i < len(sql)-1; i++ {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i++
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i++
			if depth == 0 {
				return i + 1, nil
			}
		}
	}

	return 0, fmt.Errorf("sqlsee: unterminated block comment in custom SQL")
}

func dollarQuoteDelimiter(sql string, start int) (string, bool) {
	if start+1 >= len(sql) {
		return "", false
	}
	if sql[start+1] == '$' {
		return "$$", true
	}
	if !isIdentifierStart(sql[start+1]) {
		return "", false
	}

	for i := start + 2; i < len(sql); i++ {
		if sql[i] == '$' {
			return sql[start : i+1], true
		}
		if !isIdentifierPart(sql[i]) {
			return "", false
		}
	}

	return "", false
}

func isIdentifierStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' || value >= 0x80
}

func isIdentifierPart(value byte) bool {
	return isIdentifierStart(value) || value >= '0' && value <= '9'
}
