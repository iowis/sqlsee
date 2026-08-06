package sqlsee

import (
	"context"
	"fmt"
	"reflect"
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

type Filter struct {
	Field   string `json:"field"`
	Pattern string `json:"pattern"`
}

func ILike(field, pattern string) Filter { return Filter{Field: field, Pattern: pattern} }

type FilterGroup struct {
	Any     bool     `json:"any"`
	Filters []Filter `json:"filters"`
}

func All(filters ...Filter) FilterGroup { return FilterGroup{Filters: filters} }
func Any(filters ...Filter) FilterGroup { return FilterGroup{Any: true, Filters: filters} }

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

type Lister[T any] struct {
	db DBTX

	table, alias, from, primaryKey string
	columns                        []string
	meta                           modelMeta
	filterable, sortable           map[string]struct{}
	defaultSort                    []Sort
	defaultLimit, maxLimit         int
	cursorSecret                   []byte
}

func New[T any](db DBTX, cfg Config) (*Lister[T], error) {
	if db == nil || cfg.Table == "" {
		return nil, fmt.Errorf("sqlsee: db and table are required")
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

	return &Lister[T]{
		db:    db,
		table: cfg.Table,
		alias: cfg.Alias,
		from:  table + " AS " + quote(cfg.Alias), primaryKey: cfg.PrimaryKey,
		columns: columns, meta: meta, filterable: filterable, sortable: sortable,
		defaultSort:  append([]Sort(nil), cfg.DefaultSort...),
		defaultLimit: cfg.DefaultLimit, maxLimit: cfg.MaxLimit,
		cursorSecret: append([]byte(nil), cfg.CursorSecret...),
	}, nil
}

func (l *Lister[T]) List(ctx context.Context, req Request) (Page[T], error) {
	return l.list(ctx, req, nil)
}

type rowScope struct {
	key   string
	apply func(*sqlBuilder)
}

func (l *Lister[T]) list(ctx context.Context, req Request, scope *rowScope) (Page[T], error) {
	limit := req.Limit
	if limit == 0 {
		limit = l.defaultLimit
	}

	if limit < 1 || limit > l.maxLimit {
		return Page[T]{}, fmt.Errorf("sqlsee: limit must be between 1 and %d", l.maxLimit)
	}

	sorts, err := l.resolveSort(req.Sort)
	if err != nil {
		return Page[T]{}, err
	}

	b := &sqlBuilder{}
	if err := l.addFilters(b, req.Where); err != nil {
		return Page[T]{}, err
	}

	scopeKey := ""
	if scope != nil {
		scope.apply(b)
		scopeKey = scope.key
	}

	queryKey, err := queryFingerprint(l.table, sorts, req.Where, scopeKey)
	if err != nil {
		return Page[T]{}, err
	}

	if req.Cursor != "" {
		cursor, err := decodeCursor(req.Cursor, l.cursorSecret)
		if err != nil {
			return Page[T]{}, err
		}

		if cursor.Query != queryKey || len(cursor.Keys) != len(sorts) {
			return Page[T]{}, fmt.Errorf("sqlsee: cursor does not belong to this query")
		}

		values := make([]any, len(sorts))
		for i, sort := range sorts {
			values[i], err = decodeValue(cursor.Keys[i], l.meta.fields[sort.Field].typ)
			if err != nil || isNull(values[i]) {
				return Page[T]{}, fmt.Errorf("sqlsee: invalid cursor value for %q", sort.Field)
			}
		}

		b.where = append(b.where, l.keyset(b, sorts, values))
	}

	var sql strings.Builder
	sql.WriteString("SELECT ")
	sql.WriteString(strings.Join(l.columns, ", "))
	sql.WriteString(" FROM ")
	sql.WriteString(l.from)
	if len(b.where) > 0 {
		sql.WriteString(" WHERE ")
		sql.WriteString(strings.Join(b.where, " AND "))
	}
	sql.WriteString(" ORDER BY ")
	for i, sort := range sorts {
		if i > 0 {
			sql.WriteString(", ")
		}
		sql.WriteString(l.column(sort.Field))
		if sort.Desc {
			sql.WriteString(" DESC")
		} else {
			sql.WriteString(" ASC")
		}
	}
	sql.WriteString(" LIMIT ")
	sql.WriteString(b.arg(limit + 1))

	rows, err := l.db.Query(ctx, sql.String(), b.args...)
	if err != nil {
		return Page[T]{}, fmt.Errorf("sqlsee: query: %w", err)
	}

	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
	if err != nil {
		return Page[T]{}, fmt.Errorf("sqlsee: scan: %w", err)
	}

	page := Page[T]{Items: items}
	if len(items) <= limit {
		return page, nil
	}

	page.Items, page.HasMore = items[:limit], true
	keys := make([]any, len(sorts))

	for i, sort := range sorts {
		keys[i], err = l.meta.value(page.Items[len(page.Items)-1], sort.Field)
		if err != nil || isNull(keys[i]) {
			return Page[T]{}, fmt.Errorf("sqlsee: sort field %q must be non-null", sort.Field)
		}
	}

	page.NextCursor, err = encodeCursor(queryKey, keys, l.cursorSecret)

	return page, err
}

func (l *Lister[T]) addFilters(b *sqlBuilder, groups []FilterGroup) error {
	for _, group := range groups {
		if len(group.Filters) == 0 {
			continue
		}

		parts := make([]string, 0, len(group.Filters))
		for _, filter := range group.Filters {
			if _, ok := l.filterable[filter.Field]; !ok {
				return fmt.Errorf("sqlsee: field %q is not filterable", filter.Field)
			}

			parts = append(parts, l.column(filter.Field)+" ILIKE "+b.arg(filter.Pattern))
		}

		op := " AND "
		if group.Any {
			op = " OR "
		}

		b.where = append(b.where, "("+strings.Join(parts, op)+")")
	}

	return nil
}

func (l *Lister[T]) resolveSort(requested []Sort) ([]Sort, error) {
	if len(requested) > 2 {
		return nil, fmt.Errorf("sqlsee: at most two sort fields are allowed")
	}

	if len(requested) == 0 {
		requested = l.defaultSort
	}

	seen := make(map[string]struct{}, len(requested)+1)
	result := make([]Sort, 0, len(requested)+1)

	for _, sort := range requested {
		if _, ok := l.sortable[sort.Field]; !ok {
			return nil, fmt.Errorf("sqlsee: field %q is not sortable", sort.Field)
		}

		if _, duplicate := seen[sort.Field]; duplicate {
			return nil, fmt.Errorf("sqlsee: duplicate sort field %q", sort.Field)
		}

		seen[sort.Field] = struct{}{}
		result = append(result, sort)
	}

	if _, ok := seen[l.primaryKey]; !ok {
		result = append(result, Sort{Field: l.primaryKey, Desc: result[len(result)-1].Desc})
	}

	return result, nil
}

func (l *Lister[T]) keyset(b *sqlBuilder, sorts []Sort, values []any) string {
	args := make([]string, len(values))
	for i, value := range values {
		args[i] = b.arg(value)
	}

	branches := make([]string, len(sorts))
	for i, sort := range sorts {
		terms := make([]string, 0, i+1)

		for j := range i {
			terms = append(terms, l.column(sorts[j].Field)+" = "+args[j])
		}

		op := ">"

		if sort.Desc {
			op = "<"
		}

		terms = append(terms, l.column(sort.Field)+" "+op+" "+args[i])
		branches[i] = "(" + strings.Join(terms, " AND ") + ")"
	}

	return "(" + strings.Join(branches, " OR ") + ")"
}

func (l *Lister[T]) column(name string) string {
	return quote(l.alias) + "." + quote(name)
}
