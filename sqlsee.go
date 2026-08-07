package sqlsee

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

type DBTX interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type Sort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc"`
}

func Asc(field string) Sort  { return Sort{Field: field} }
func Desc(field string) Sort { return Sort{Field: field, Desc: true} }

type FilterOperator string

const (
	FilterOperatorILike  FilterOperator = "ilike"
	FilterOperatorLike   FilterOperator = "like"
	FilterOperatorEquals FilterOperator = "equals"
)

type Filter struct {
	Field    string         `json:"field"`
	Operator FilterOperator `json:"operator,omitempty"`
	Value    any            `json:"value,omitempty"`

	// Pattern is retained for compatibility with Filter literals from earlier
	// releases. New code should use ILike, Like, or Equals.
	Pattern string `json:"pattern,omitempty"`
}

func ILike(field, pattern string) Filter {
	return Filter{Field: field, Pattern: pattern}
}

func Like(field, pattern string) Filter {
	return Filter{Field: field, Operator: FilterOperatorLike, Value: pattern}
}

func Equals(field string, value any) Filter {
	return Filter{Field: field, Operator: FilterOperatorEquals, Value: value}
}

type FilterGroup struct {
	Any     bool     `json:"any"`
	Filters []Filter `json:"filters"`
}

func All(filters ...Filter) FilterGroup {
	return FilterGroup{Filters: filters}
}

func Any(filters ...Filter) FilterGroup {
	return FilterGroup{Any: true, Filters: filters}
}

type Request struct {
	Limit  int
	Cursor string
	Where  []FilterGroup
	Sort   []Sort
}

type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}

type Config struct {
	Table        string
	Alias        string
	PrimaryKey   string
	Filterable   []string
	Sortable     []string
	DefaultSort  []Sort
	DefaultLimit int
	MaxLimit     int
	CursorSecret []byte
}

type Query[T any] struct {
	db DBTX

	table, alias, from, primaryKey string
	columns                        []string
	meta                           modelMeta
	filterable, sortable           map[string]struct{}
	defaultSort                    []Sort
	defaultLimit, maxLimit         int
	cursorSecret                   []byte
	plugins                        []installedPlugin
	pluginNames                    map[string]struct{}
}

func New[T any](db DBTX, cfg Config, options ...Option) (*Query[T], error) {
	if db == nil {
		return nil, fmt.Errorf("sqlsee: db is required")
	}

	if cfg.Table == "" {
		return nil, fmt.Errorf("sqlsee: table is required")
	}

	if cfg.Alias == "" {
		cfg.Alias = "t"
	}

	if cfg.PrimaryKey == "" {
		cfg.PrimaryKey = "id"
	}

	if cfg.DefaultLimit <= 0 {
		cfg.DefaultLimit = 50
	}

	if cfg.MaxLimit <= 0 {
		cfg.MaxLimit = 100
	}

	if cfg.DefaultLimit > cfg.MaxLimit {
		return nil, fmt.Errorf("sqlsee: default limit exceeds max limit")
	}

	typ := reflect.TypeFor[T]()
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("sqlsee: T must be a struct, got %s", typ)
	}

	meta := metadataFor(typ)
	if meta.err != nil {
		return nil, meta.err
	}

	if _, ok := meta.fields[cfg.PrimaryKey]; !ok {
		return nil, fmt.Errorf("sqlsee: unknown primary key %q", cfg.PrimaryKey)
	}

	filterable, err := allowedSet(cfg.Filterable, meta)
	if err != nil {
		return nil, err
	}

	sortable, err := allowedSet(cfg.Sortable, meta)
	if err != nil {
		return nil, err
	}

	sortable[cfg.PrimaryKey] = struct{}{}
	if len(cfg.DefaultSort) == 0 {
		cfg.DefaultSort = []Sort{Asc(cfg.PrimaryKey)}
	}

	if len(cfg.DefaultSort) > 2 {
		return nil, fmt.Errorf("sqlsee: default sort supports at most two fields")
	}

	for _, sort := range cfg.DefaultSort {
		if _, ok := sortable[sort.Field]; !ok {
			return nil, fmt.Errorf("sqlsee: field %q is not sortable", sort.Field)
		}
	}

	table, err := quoteQualified(cfg.Table)
	if err != nil {
		return nil, err
	}

	columns, err := ColumnsFor[T](cfg.Alias)
	if err != nil {
		return nil, err
	}

	query := &Query[T]{
		db:           db,
		table:        cfg.Table,
		alias:        cfg.Alias,
		from:         table + " AS " + quote(cfg.Alias),
		primaryKey:   cfg.PrimaryKey,
		columns:      columns,
		meta:         meta,
		filterable:   filterable,
		sortable:     sortable,
		defaultSort:  append([]Sort(nil), cfg.DefaultSort...),
		defaultLimit: cfg.DefaultLimit,
		maxLimit:     cfg.MaxLimit,
		cursorSecret: append([]byte(nil), cfg.CursorSecret...),
		pluginNames:  make(map[string]struct{}, len(options)),
	}

  if len(options) > 0 {
    if err := query.AddOptions(options...); err != nil {
      return nil, err
    }
  }

	return query, nil
}

func (q *Query[T]) Clone() *Query[T] {
	return &Query[T]{
		db:           q.db,
		table:        q.table,
		alias:        q.alias,
		from:         q.from,
		primaryKey:   q.primaryKey,
		columns:      slices.Clone(q.columns),
		meta:         q.meta,
		filterable:   maps.Clone(q.filterable),
		sortable:     maps.Clone(q.sortable),
		defaultSort:  slices.Clone(q.defaultSort),
		defaultLimit: q.defaultLimit,
		maxLimit:     q.maxLimit,
		cursorSecret: bytes.Clone(q.cursorSecret),
		pluginNames:  maps.Clone(q.pluginNames),
	}
}

func (q *Query[T]) AddOptions(options ...Option) error {
	pluginFields := make(map[string]struct{}, len(q.meta.fields))
	for field := range q.meta.fields {
		pluginFields[field] = struct{}{}
	}

	pluginContext := PluginContext{
		primaryKey: q.primaryKey,
		alias:      q.alias,
		fields:     pluginFields,
	}

	for _, option := range options {
		plugin := option.plugin
		if isNilPlugin(plugin) {
			return fmt.Errorf("sqlsee: nil plugin")
		}

		name := plugin.Name()
		if strings.TrimSpace(name) == "" {
			return  fmt.Errorf("sqlsee: plugin name is required")
		}
		if _, duplicate := q.pluginNames[name]; duplicate {
			return  fmt.Errorf("sqlsee: duplicate plugin %q", name)
		}

		handler, err := plugin.Install(pluginContext)
		if err != nil {
			return fmt.Errorf("sqlsee: install plugin %q: %w", name, err)
		}
		if handler == nil {
			return fmt.Errorf("sqlsee: plugin %q returned a nil handler", name)
		}

		q.pluginNames[name] = struct{}{}
		q.plugins = append(q.plugins, installedPlugin{
			name:    name,
			handler: handler,
		})
	}

	return nil
}

func (q *Query[T]) List(
	ctx context.Context,
	req Request,
	options ...ListOption,
) (Page[T], error) {
	page, err := q.list(ctx, req, options, false)
	if err != nil {
		return Page[T]{}, err
	}
	return page.Page, nil
}

// PageWithExtras is the result of ListWithExtras. Each entry in Extras holds
// the projected columns contributed by plugins for the item at the same index
// in Items, keyed by the alias the plugin declared.
type PageWithExtras[T any] struct {
	Items      []T
	Extras     []map[string]any
	NextCursor string
	HasMore    bool
}

// ListWithExtras behaves like List but also returns plugin-provided projection
// columns. Plugins that contributed Select columns populate the per-row
// Extras map keyed by the declared alias. Plugins that only contribute Join or
// Where clauses leave Extras empty for that row.
func (q *Query[T]) ListWithExtras(
	ctx context.Context,
	req Request,
	options ...ListOption,
) (PageWithExtras[T], error) {
	page, err := q.list(ctx, req, options, true)
	if err != nil {
		return PageWithExtras[T]{}, err
	}
	return PageWithExtras[T]{
		Items:      page.Page.Items,
		Extras:     page.Extras,
		NextCursor: page.Page.NextCursor,
		HasMore:    page.Page.HasMore,
	}, nil
}

type fullPage[T any] struct {
	Page   Page[T]
	Extras []map[string]any
}

func (q *Query[T]) list(
	ctx context.Context,
	req Request,
	options []ListOption,
	collectExtras bool,
) (fullPage[T], error) {
	limit := req.Limit
	if limit == 0 {
		limit = q.defaultLimit
	}

	if limit < 1 || limit > q.maxLimit {
		return fullPage[T]{}, fmt.Errorf("sqlsee: limit must be between 1 and %d", q.maxLimit)
	}

	sorts, err := q.resolveSort(req.Sort)
	if err != nil {
		return fullPage[T]{}, err
	}

	inputs := make(map[string]PluginInput, len(options))
	for _, option := range options {
		if strings.TrimSpace(option.plugin) == "" {
			return fullPage[T]{}, fmt.Errorf("sqlsee: plugin option name is required")
		}
		if _, installed := q.pluginNames[option.plugin]; !installed {
			return fullPage[T]{}, fmt.Errorf("sqlsee: plugin %q is not installed", option.plugin)
		}
		if _, duplicate := inputs[option.plugin]; duplicate {
			return fullPage[T]{}, fmt.Errorf("sqlsee: duplicate option for plugin %q", option.plugin)
		}
		inputs[option.plugin] = PluginInput{present: true, value: option.value}
	}

	results := make([]pluginResult, 0, len(q.plugins))
	for _, plugin := range q.plugins {
		input := inputs[plugin.name]
		builder := &PluginBuilder{}
		if err := plugin.handler(ctx, input, builder); err != nil {
			return fullPage[T]{}, fmt.Errorf("sqlsee: plugin %q: %w", plugin.name, err)
		}
		results = append(results, pluginResult{
			name:    plugin.name,
			clauses: append([]sqlClause(nil), builder.clauses...),
		})
	}

	b := &sqlBuilder{}
	joins, selects, pluginScope, err := applyPluginResults(b, results)
	if err != nil {
		return fullPage[T]{}, err
	}
	if err := q.addFilters(b, req.Where); err != nil {
		return fullPage[T]{}, err
	}

	queryKey, err := queryFingerprint(q.table, sorts, req.Where, pluginScope)
	if err != nil {
		return fullPage[T]{}, err
	}

	if req.Cursor != "" {
		cursor, err := decodeCursor(req.Cursor, q.cursorSecret)
		if err != nil {
			return fullPage[T]{}, err
		}

		if cursor.Query != queryKey || len(cursor.Keys) != len(sorts) {
			return fullPage[T]{}, fmt.Errorf("sqlsee: cursor does not belong to this query")
		}

		values := make([]any, len(sorts))
		for i, sort := range sorts {
			values[i], err = decodeValue(cursor.Keys[i], q.meta.fields[sort.Field].typ)
			if err != nil || isNull(values[i]) {
				return fullPage[T]{}, fmt.Errorf("sqlsee: invalid cursor value for %q", sort.Field)
			}
		}

		b.where = append(b.where, q.keyset(b, sorts, values))
	}

	selectList := strings.Join(q.columns, ", ")
	if len(selects) > 0 {
		parts := make([]string, len(selects))
		for i, sel := range selects {
			if _, ok := q.meta.fields[sel.alias]; ok {
				return fullPage[T]{}, fmt.Errorf(
					"sqlsee: plugin select alias %q collides with model field", sel.alias)
			}
			alias, _ := QuoteIdentifier(sel.alias)
			parts[i] = sel.sql + " AS " + alias
		}
		selectList += ", " + strings.Join(parts, ", ")
	}

	var sql strings.Builder
	sql.WriteString("SELECT ")
	sql.WriteString(selectList)
	sql.WriteString(" FROM ")
	sql.WriteString(q.from)
	if len(joins) > 0 {
		sql.WriteByte(' ')
		sql.WriteString(strings.Join(joins, " "))
	}
	if len(b.where) > 0 {
		sql.WriteString(" WHERE ")
		sql.WriteString(strings.Join(b.where, " AND "))
	}
	sql.WriteString(" ORDER BY ")
	for i, sort := range sorts {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteString(q.column(sort.Field))
		if sort.Desc {
			sql.WriteString(" DESC")
		} else {
			sql.WriteString(" ASC")
		}
	}
	sql.WriteString(" LIMIT ")
	sql.WriteString(b.arg(limit + 1))

	rows, err := q.db.Query(ctx, sql.String(), b.args...)
	if err != nil {
		return fullPage[T]{}, fmt.Errorf("sqlsee: query: %w", err)
	}

	items, extras, err := collectRows[T](rows, q.meta, collectExtras)
	if err != nil {
		return fullPage[T]{}, fmt.Errorf("sqlsee: scan: %w", err)
	}

	page := Page[T]{Items: items}
	result := fullPage[T]{Page: page, Extras: extras}
	if len(items) <= limit {
		return result, nil
	}

	page.Items, page.HasMore = items[:limit], true
	result.Page = page
	if collectExtras {
		result.Extras = extras[:limit]
	}
	keys := make([]any, len(sorts))

	for i, sort := range sorts {
		keys[i], err = q.meta.value(page.Items[len(page.Items)-1], sort.Field)
		if err != nil || isNull(keys[i]) {
			return fullPage[T]{}, fmt.Errorf("sqlsee: sort field %q must be non-null", sort.Field)
		}
	}

	page.NextCursor, err = encodeCursor(queryKey, keys, q.cursorSecret)
	result.Page = page

	return result, err
}

func (q *Query[T]) addFilters(b *sqlBuilder, groups []FilterGroup) error {
	for _, group := range groups {
		if len(group.Filters) == 0 {
			continue
		}

		parts := make([]string, 0, len(group.Filters))
		for _, filter := range group.Filters {
			if _, ok := q.filterable[filter.Field]; !ok {
				return fmt.Errorf("sqlsee: field %q is not filterable", filter.Field)
			}

			operator := filter.Operator
			value := filter.Value
			if operator == "" {
				operator = FilterOperatorILike
			}
			if operator != FilterOperatorEquals && value == nil {
				value = filter.Pattern
			}

			sqlOperator := ""
			switch operator {
			case FilterOperatorILike:
				sqlOperator = "ILIKE"
			case FilterOperatorLike:
				sqlOperator = "LIKE"
			case FilterOperatorEquals:
				sqlOperator = "="
			default:
				return fmt.Errorf("sqlsee: unsupported filter operator %q", operator)
			}

			if operator == FilterOperatorEquals && isNull(value) {
				parts = append(parts, q.column(filter.Field)+" IS NULL")
				continue
			}

			parts = append(parts, q.column(filter.Field)+" "+sqlOperator+" "+b.arg(value))
		}

		op := " AND "
		if group.Any {
			op = " OR "
		}

		b.where = append(b.where, "("+strings.Join(parts, op)+")")
	}

	return nil
}

func (q *Query[T]) resolveSort(requested []Sort) ([]Sort, error) {
	if len(requested) > 2 {
		return nil, fmt.Errorf("sqlsee: at most two sort fields are allowed")
	}

	if len(requested) == 0 {
		requested = q.defaultSort
	}

	seen := make(map[string]struct{}, len(requested)+1)
	result := make([]Sort, 0, len(requested)+1)

	for _, sort := range requested {
		if _, ok := q.sortable[sort.Field]; !ok {
			return nil, fmt.Errorf("sqlsee: field %q is not sortable", sort.Field)
		}

		if _, duplicate := seen[sort.Field]; duplicate {
			return nil, fmt.Errorf("sqlsee: duplicate sort field %q", sort.Field)
		}

		seen[sort.Field] = struct{}{}
		result = append(result, sort)
	}

	if _, ok := seen[q.primaryKey]; !ok {
		result = append(result, Sort{
			Field: q.primaryKey,
			Desc:  result[len(result)-1].Desc,
		})
	}

	return result, nil
}

func (q *Query[T]) keyset(b *sqlBuilder, sorts []Sort, values []any) string {
	args := make([]string, len(values))
	for i, value := range values {
		args[i] = b.arg(value)
	}

	branches := make([]string, len(sorts))
	for i, sort := range sorts {
		terms := make([]string, 0, i+1)

		for j := range i {
			terms = append(terms, q.column(sorts[j].Field)+" = "+args[j])
		}

		op := ">"

		if sort.Desc {
			op = "<"
		}

		terms = append(terms, q.column(sort.Field)+" "+op+" "+args[i])
		branches[i] = "(" + strings.Join(terms, " AND ") + ")"
	}

	return "(" + strings.Join(branches, " OR ") + ")"
}

func (q *Query[T]) column(name string) string {
	return quote(q.alias) + "." + quote(name)
}
