package sqlsee

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

type queryModel struct {
	ID     int    `db:"id"`
	Name   string `db:"name"`
	Active bool   `db:"active"`
}

type testPlugin struct {
	name    string
	install func(PluginContext) (PluginHandler, error)
}

func (p testPlugin) Name() string { return p.name }

func (p testPlugin) Install(ctx PluginContext) (PluginHandler, error) {
	if p.install == nil {
		return func(context.Context, PluginInput, *PluginBuilder) error { return nil }, nil
	}

	return p.install(ctx)
}

type pointerPlugin struct{}

func (*pointerPlugin) Name() string { return "pointer" }

func (*pointerPlugin) Install(PluginContext) (PluginHandler, error) {
	return func(context.Context, PluginInput, *PluginBuilder) error { return nil }, nil
}

type functionPlugin func()

func (functionPlugin) Name() string { return "function" }

func (functionPlugin) Install(PluginContext) (PluginHandler, error) {
	return func(context.Context, PluginInput, *PluginBuilder) error { return nil }, nil
}

type recordedQuery struct {
	sql  string
	args []any
}

type recordingDB struct {
	queries []recordedQuery
	rows    []pgx.Rows
	err     error
}

func (db *recordingDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.queries = append(db.queries, recordedQuery{sql: sql, args: append([]any(nil), args...)})
	if db.err != nil {
		return nil, db.err
	}
	if len(db.rows) == 0 {
		return newFakeRows(nil), nil
	}

	rows := db.rows[0]
	db.rows = db.rows[1:]
	return rows, nil
}

type fakeRows struct {
	fields  []pgconn.FieldDescription
	rows    [][]any
	current []any
	next    int
	closed  bool
	err     error
}

func newFakeRows(rows [][]any) *fakeRows {
	return &fakeRows{
		fields: []pgconn.FieldDescription{
			{Name: "id"},
			{Name: "name"},
			{Name: "active"},
		},
		rows: rows,
	}
}

func (r *fakeRows) Close() { r.closed = true }

func (r *fakeRows) Err() error { return r.err }

func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return r.fields }

func (r *fakeRows) Next() bool {
	if r.next >= len(r.rows) {
		r.Close()
		return false
	}

	r.current = r.rows[r.next]
	r.next++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if len(dest) != len(r.current) {
		return errors.New("unexpected scan destination count")
	}

	for i := range dest {
		if dest[i] == nil {
			continue
		}

		value := reflect.ValueOf(dest[i])
		if value.Kind() != reflect.Pointer || value.IsNil() {
			return errors.New("scan destination is not a pointer")
		}
		if r.current[i] == nil {
			value.Elem().SetZero()
			continue
		}

		value.Elem().Set(reflect.ValueOf(r.current[i]))
	}

	return nil
}

func (r *fakeRows) Values() ([]any, error) {
	return append([]any(nil), r.current...), nil
}

func (r *fakeRows) RawValues() [][]byte { return nil }

func (r *fakeRows) Conn() *pgx.Conn { return nil }

func newQuery(t *testing.T, db DBTX, mutate ...func(*Config)) *Query[queryModel] {
	t.Helper()

	cfg := Config{
		Table:        "public.users",
		Alias:        "u",
		Filterable:   []string{"name", "active"},
		Sortable:     []string{"name"},
		DefaultSort:  []Sort{Asc("name")},
		DefaultLimit: 10,
		MaxLimit:     20,
	}
	for _, fn := range mutate {
		fn(&cfg)
	}

	query, err := New[queryModel](db, cfg)
	require.NoError(t, err)
	return query
}

func newQueryWithPlugins(t *testing.T, db DBTX, plugins ...Plugin) *Query[queryModel] {
	t.Helper()

	options := make([]Option, len(plugins))
	for i, plugin := range plugins {
		options[i] = WithPlugin(plugin)
	}
	query, err := New[queryModel](db, Config{
		Table:        "public.users",
		Alias:        "u",
		Filterable:   []string{"name", "active"},
		Sortable:     []string{"name"},
		DefaultSort:  []Sort{Asc("name")},
		DefaultLimit: 10,
		MaxLimit:     20,
	}, options...)
	require.NoError(t, err)
	return query
}

func TestQueryListBuildsFilterOperators(t *testing.T) {
	db := &recordingDB{rows: []pgx.Rows{newFakeRows([][]any{{1, "Gopher", true}})}}
	query := newQuery(t, db)

	page, err := query.List(t.Context(), Request{
		Limit: 10,
		Where: []FilterGroup{All(
			ILike("name", "%go%"),
			Like("name", "Go%"),
			Equals("active", true),
		)},
	})

	require.NoError(t, err)
	require.Equal(t, []queryModel{{ID: 1, Name: "Gopher", Active: true}}, page.Items)
	require.False(t, page.HasMore)
	require.Equal(t,
		`SELECT "u"."id" AS "id", "u"."name" AS "name", "u"."active" AS "active" FROM "public"."users" AS "u" WHERE ("u"."name" ILIKE $1 AND "u"."name" LIKE $2 AND "u"."active" = $3) ORDER BY "u"."name" ASC, "u"."id" ASC LIMIT $4`,
		db.queries[0].sql,
	)
	require.Equal(t, []any{"%go%", "Go%", true, 11}, db.queries[0].args)
}

func TestQueryListSupportsAnyAndLegacyILikeFilters(t *testing.T) {
	db := &recordingDB{}
	query := newQuery(t, db)

	_, err := query.List(t.Context(), Request{
		Where: []FilterGroup{
			Any(
				Filter{Field: "name", Pattern: "%old%"},
				Equals("active", false),
			),
			{},
		},
	})

	require.NoError(t, err)
	require.Contains(t, db.queries[0].sql,
		`WHERE ("u"."name" ILIKE $1 OR "u"."active" = $2) ORDER BY`,
	)
	require.Equal(t, []any{"%old%", false, 11}, db.queries[0].args)
}

func TestQueryListEqualsNilUsesIsNull(t *testing.T) {
	db := &recordingDB{}
	query := newQuery(t, db)

	_, err := query.List(t.Context(), Request{
		Where: []FilterGroup{All(Equals("name", nil))},
	})

	require.NoError(t, err)
	require.Contains(t, db.queries[0].sql, `WHERE ("u"."name" IS NULL) ORDER BY`)
	require.Equal(t, []any{11}, db.queries[0].args)
}

func TestQueryListAppliesPluginSQL(t *testing.T) {
	db := &recordingDB{}
	plugin := testPlugin{name: "test.sql", install: func(ctx PluginContext) (PluginHandler, error) {
		id, err := ctx.Column("id")
		require.NoError(t, err)
		return func(_ context.Context, input PluginInput, builder *PluginBuilder) error {
			value := input.Value().(struct {
				role   string
				active bool
			})
			if err := builder.Join(
				`LEFT JOIN memberships AS m ON m.user_id = `+id+` AND m.tenant_id = $1`,
				42,
			); err != nil {
				return err
			}
			if err := builder.Where(`m.deleted_at IS NULL`); err != nil {
				return err
			}
			if err := builder.Join(
				`JOIN roles AS r ON r.user_id = `+id+` AND r.name = $1`,
				value.role,
			); err != nil {
				return err
			}
			return builder.Where(`r.active = $1`, value.active)
		}, nil
	}}
	query := newQueryWithPlugins(t, db, plugin)
	pluginValue := struct {
		role   string
		active bool
	}{"admin", true}

	_, err := query.List(t.Context(), Request{
		Limit: 5,
		Where: []FilterGroup{
			All(Equals("active", true)),
		},
	}, NewPluginOption("test.sql", pluginValue))

	require.NoError(t, err)
	require.Equal(t,
		`SELECT "u"."id" AS "id", "u"."name" AS "name", "u"."active" AS "active" FROM "public"."users" AS "u" LEFT JOIN memberships AS m ON m.user_id = "u"."id" AND m.tenant_id = $1 JOIN roles AS r ON r.user_id = "u"."id" AND r.name = $2 WHERE (m.deleted_at IS NULL) AND (r.active = $3) AND ("u"."active" = $4) ORDER BY "u"."name" ASC, "u"."id" ASC LIMIT $5`,
		db.queries[0].sql,
	)
	require.Equal(t, []any{42, "admin", true, true, 6}, db.queries[0].args)
}

func TestQueryFingerprintPreservesLegacyILikeCursors(t *testing.T) {
	fingerprint, err := queryFingerprint(
		"public.users",
		[]Sort{Asc("name"), Asc("id")},
		[]FilterGroup{All(ILike("name", "%go%"))},
		"",
	)

	require.NoError(t, err)
	require.Equal(t, "61979f3bb348c3f12fda0688258564ecc26ee7cea4bd0d150008a93804f4fc3e", fingerprint)
}

func TestQueryListPaginatesWithCursor(t *testing.T) {
	db := &recordingDB{rows: []pgx.Rows{
		newFakeRows([][]any{{1, "Alpha", true}, {2, "Beta", false}}),
		newFakeRows([][]any{{2, "Beta", false}}),
	}}
	query := newQuery(t, db)
	req := Request{
		Limit: 1,
		Where: []FilterGroup{
			All(Equals("active", true)),
		},
	}

	first, err := query.List(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, []queryModel{{ID: 1, Name: "Alpha", Active: true}}, first.Items)
	require.True(t, first.HasMore)
	require.NotEmpty(t, first.NextCursor)

	req.Cursor = first.NextCursor
	second, err := query.List(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, []queryModel{{ID: 2, Name: "Beta", Active: false}}, second.Items)
	require.False(t, second.HasMore)
	require.Contains(t, db.queries[1].sql,
		`WHERE ("u"."active" = $1) AND (("u"."name" > $2) OR ("u"."name" = $2 AND "u"."id" > $3))`,
	)
	require.Equal(t, []any{true, "Alpha", 1, 2}, db.queries[1].args)
}

func TestQueryCursorIsBoundToPluginArguments(t *testing.T) {
	firstDB := &recordingDB{rows: []pgx.Rows{
		newFakeRows([][]any{{1, "Alpha", true}, {2, "Beta", true}}),
		newFakeRows([][]any{{2, "Beta", true}}),
	}}
	plugin := testPlugin{name: "test.scope", install: func(PluginContext) (PluginHandler, error) {
		return func(_ context.Context, input PluginInput, builder *PluginBuilder) error {
			return builder.Where(`"u"."id" <> $1`, input.Value())
		}, nil
	}}
	query := newQueryWithPlugins(t, firstDB, plugin)
	page, err := query.List(
		t.Context(),
		Request{Limit: 1},
		NewPluginOption("test.scope", 1),
	)
	require.NoError(t, err)

	next, err := query.List(
		t.Context(),
		Request{Limit: 1, Cursor: page.NextCursor},
		NewPluginOption("test.scope", 1),
	)
	require.NoError(t, err)
	require.Equal(t, []queryModel{{ID: 2, Name: "Beta", Active: true}}, next.Items)
	require.Contains(t, firstDB.queries[1].sql,
		`WHERE ("u"."id" <> $1) AND (("u"."name" > $2) OR ("u"."name" = $2 AND "u"."id" > $3))`,
	)
	require.Equal(t, []any{1, "Alpha", 1, 2}, firstDB.queries[1].args)

	_, err = query.List(
		t.Context(),
		Request{Limit: 1, Cursor: page.NextCursor},
		NewPluginOption("test.scope", 2),
	)

	require.EqualError(t, err, "sqlsee: cursor does not belong to this query")
	require.Len(t, firstDB.queries, 2)
}

func TestQueryValidation(t *testing.T) {
	t.Run("requires database", func(t *testing.T) {
		query, err := New[queryModel](nil, Config{Table: "users"})
		require.Nil(t, query)
		require.EqualError(t, err, "sqlsee: db is required")
	})

	t.Run("requires table", func(t *testing.T) {
		query, err := New[queryModel](&recordingDB{}, Config{})
		require.Nil(t, query)
		require.EqualError(t, err, "sqlsee: table is required")
	})
}

func TestPluginContextMetadata(t *testing.T) {
	plugin := testPlugin{name: "test.metadata", install: func(ctx PluginContext) (PluginHandler, error) {
		require.Equal(t, "id", ctx.PrimaryKey())
		column, err := ctx.Column("name")
		require.NoError(t, err)
		require.Equal(t, `"u"."name"`, column)
		column, err = ctx.Column("missing")
		require.Empty(t, column)
		require.EqualError(t, err, `sqlsee: unknown field "missing"`)
		return func(context.Context, PluginInput, *PluginBuilder) error { return nil }, nil
	}}

	newQueryWithPlugins(t, &recordingDB{}, plugin)
}

func TestPluginConstructionValidation(t *testing.T) {
	newWith := func(options ...Option) (*Query[queryModel], error) {
		return New[queryModel](&recordingDB{}, Config{Table: "users"}, options...)
	}

	t.Run("nil plugin", func(t *testing.T) {
		query, err := newWith(WithPlugin(nil))
		require.Nil(t, query)
		require.EqualError(t, err, "sqlsee: nil plugin")
	})

	t.Run("typed nil plugin", func(t *testing.T) {
		var plugin *pointerPlugin
		query, err := newWith(WithPlugin(plugin))
		require.Nil(t, query)
		require.EqualError(t, err, "sqlsee: nil plugin")
	})

	t.Run("typed nil function plugin", func(t *testing.T) {
		var plugin functionPlugin
		query, err := newWith(WithPlugin(plugin))
		require.Nil(t, query)
		require.EqualError(t, err, "sqlsee: nil plugin")
	})

	t.Run("empty name", func(t *testing.T) {
		query, err := newWith(WithPlugin(testPlugin{name: " \t"}))
		require.Nil(t, query)
		require.EqualError(t, err, "sqlsee: plugin name is required")
	})

	t.Run("duplicate name", func(t *testing.T) {
		query, err := newWith(
			WithPlugin(testPlugin{name: "test.duplicate"}),
			WithPlugin(testPlugin{name: "test.duplicate"}),
		)
		require.Nil(t, query)
		require.EqualError(t, err, `sqlsee: duplicate plugin "test.duplicate"`)
	})

	t.Run("install error", func(t *testing.T) {
		plugin := testPlugin{name: "test.install", install: func(PluginContext) (PluginHandler, error) {
			return nil, errors.New("broken configuration")
		}}
		query, err := newWith(WithPlugin(plugin))
		require.Nil(t, query)
		require.EqualError(t, err,
			`sqlsee: install plugin "test.install": broken configuration`)
	})

	t.Run("nil handler", func(t *testing.T) {
		plugin := testPlugin{name: "test.nil-handler", install: func(PluginContext) (PluginHandler, error) {
			return nil, nil
		}}
		query, err := newWith(WithPlugin(plugin))
		require.Nil(t, query)
		require.EqualError(t, err,
			`sqlsee: plugin "test.nil-handler" returned a nil handler`)
	})
}

func TestPluginsRunInInstallationOrderWithoutOptions(t *testing.T) {
	var calls []string
	plugin := func(name string) testPlugin {
		return testPlugin{name: name, install: func(PluginContext) (PluginHandler, error) {
			return func(_ context.Context, input PluginInput, _ *PluginBuilder) error {
				require.False(t, input.Present())
				require.Nil(t, input.Value())
				calls = append(calls, name)
				return nil
			}, nil
		}}
	}
	query := newQueryWithPlugins(t, &recordingDB{}, plugin("test.first"), plugin("test.second"))

	_, err := query.List(t.Context(), Request{})

	require.NoError(t, err)
	require.Equal(t, []string{"test.first", "test.second"}, calls)
}

func TestPluginInstallsOnceAndHandlesEveryListCall(t *testing.T) {
	installs := 0
	handles := 0
	plugin := testPlugin{name: "test.lifecycle", install: func(PluginContext) (PluginHandler, error) {
		installs++
		return func(context.Context, PluginInput, *PluginBuilder) error {
			handles++
			return nil
		}, nil
	}}
	query := newQueryWithPlugins(t, &recordingDB{}, plugin)

	_, firstErr := query.List(t.Context(), Request{})
	_, secondErr := query.List(t.Context(), Request{})

	require.NoError(t, firstErr)
	require.NoError(t, secondErr)
	require.Equal(t, 1, installs)
	require.Equal(t, 2, handles)
}

func TestPluginListOptionValidation(t *testing.T) {
	plugin := testPlugin{name: "test.option"}

	t.Run("empty name", func(t *testing.T) {
		query := newQueryWithPlugins(t, &recordingDB{}, plugin)
		_, err := query.List(t.Context(), Request{}, ListOption{})
		require.EqualError(t, err, "sqlsee: plugin option name is required")
	})

	t.Run("unknown plugin", func(t *testing.T) {
		query := newQuery(t, &recordingDB{})
		_, err := query.List(t.Context(), Request{}, NewPluginOption("test.unknown", 1))
		require.EqualError(t, err, `sqlsee: plugin "test.unknown" is not installed`)
	})

	t.Run("duplicate option", func(t *testing.T) {
		query := newQueryWithPlugins(t, &recordingDB{}, plugin)
		_, err := query.List(
			t.Context(),
			Request{},
			NewPluginOption("test.option", 1),
			NewPluginOption("test.option", 2),
		)
		require.EqualError(t, err, `sqlsee: duplicate option for plugin "test.option"`)
	})
}

func TestPluginHandlerErrorsAreWrapped(t *testing.T) {
	plugin := testPlugin{name: "test.handler", install: func(PluginContext) (PluginHandler, error) {
		return func(context.Context, PluginInput, *PluginBuilder) error {
			return errors.New("denied")
		}, nil
	}}
	query := newQueryWithPlugins(t, &recordingDB{}, plugin)

	_, err := query.List(t.Context(), Request{})

	require.EqualError(t, err, `sqlsee: plugin "test.handler": denied`)
}

func TestPluginBuilderErrorsAreWrapped(t *testing.T) {
	plugin := testPlugin{name: "test.builder", install: func(PluginContext) (PluginHandler, error) {
		return func(_ context.Context, _ PluginInput, builder *PluginBuilder) error {
			return builder.Where("WHERE active = $1", true)
		}, nil
	}}
	query := newQueryWithPlugins(t, &recordingDB{}, plugin)

	_, err := query.List(t.Context(), Request{})

	require.EqualError(t, err,
		`sqlsee: plugin "test.builder": sqlsee: plugin Where must not include the WHERE keyword`)
}

func TestQueryRequestValidation(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		err  string
	}{
		{
			name: "limit",
			req:  Request{Limit: 21},
			err:  "sqlsee: limit must be between 1 and 20",
		},
		{
			name: "filter field",
			req:  Request{Where: []FilterGroup{All(Equals("id", 1))}},
			err:  `sqlsee: field "id" is not filterable`,
		},
		{
			name: "filter operator",
			req: Request{Where: []FilterGroup{All(Filter{
				Field: "name", Operator: "contains", Value: "go",
			})}},
			err: `sqlsee: unsupported filter operator "contains"`,
		},
		{
			name: "sort field",
			req:  Request{Sort: []Sort{Asc("active")}},
			err:  `sqlsee: field "active" is not sortable`,
		},
		{
			name: "duplicate sort",
			req:  Request{Sort: []Sort{Asc("name"), Desc("name")}},
			err:  `sqlsee: duplicate sort field "name"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &recordingDB{}
			query := newQuery(t, db)

			_, err := query.List(t.Context(), tt.req)

			require.EqualError(t, err, tt.err)
			require.Empty(t, db.queries)
		})
	}
}

func newFakeRowsWithExtra(rows [][]any) *fakeRows {
	return &fakeRows{
		fields: []pgconn.FieldDescription{
			{Name: "id"},
			{Name: "name"},
			{Name: "active"},
			{Name: "extra"},
		},
		rows: rows,
	}
}

func TestListWithExtrasReturnsProjectedColumns(t *testing.T) {
	pluginName := "test/projection"
	projPlugin := testPlugin{
		name: pluginName,
		install: func(ctx PluginContext) (PluginHandler, error) {
			return func(_ context.Context, _ PluginInput, b *PluginBuilder) error {
				return b.Select(`$1::text`, "extra", "value")
			}, nil
		},
	}

	db := &recordingDB{rows: []pgx.Rows{
		newFakeRowsWithExtra([][]any{
			{1, "alice", true, "hello"},
			{2, "bob", false, "world"},
		}),
	}}
	query := newQueryWithPlugins(t, db, projPlugin)

	page, err := query.ListWithExtras(t.Context(), Request{})

	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, queryModel{ID: 1, Name: "alice", Active: true}, page.Items[0])
	require.Equal(t, queryModel{ID: 2, Name: "bob", Active: false}, page.Items[1])
	require.Len(t, page.Extras, 2)
	require.Equal(t, "hello", page.Extras[0]["extra"])
	require.Equal(t, "world", page.Extras[1]["extra"])
	require.Contains(t, db.queries[0].sql, ` AS "extra"`)
}

func TestListIgnoresSelectColumnsWhenNotCollectingExtras(t *testing.T) {
	pluginName := "test/projection-ignored"
	projPlugin := testPlugin{
		name: pluginName,
		install: func(ctx PluginContext) (PluginHandler, error) {
			return func(_ context.Context, _ PluginInput, b *PluginBuilder) error {
				return b.Select(`$1::text`, "extra", "value")
			}, nil
		},
	}

	db := &recordingDB{rows: []pgx.Rows{
		newFakeRows([][]any{
			{1, "alice", true},
		}),
	}}
	query := newQueryWithPlugins(t, db, projPlugin)

	page, err := query.List(t.Context(), Request{})

	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, queryModel{ID: 1, Name: "alice", Active: true}, page.Items[0])
	require.Contains(t, db.queries[0].sql, ` AS "extra"`)
}

func TestListWithExtrasRejectsAliasCollision(t *testing.T) {
	pluginName := "test/collision"
	collisionPlugin := testPlugin{
		name: pluginName,
		install: func(ctx PluginContext) (PluginHandler, error) {
			return func(_ context.Context, _ PluginInput, b *PluginBuilder) error {
				return b.Select(`$1::int`, "name", 1)
			}, nil
		},
	}

	db := &recordingDB{}
	query := newQueryWithPlugins(t, db, collisionPlugin)

	page, err := query.ListWithExtras(t.Context(), Request{})

	require.Empty(t, page.Items)
	require.EqualError(t, err,
		`sqlsee: plugin select alias "name" collides with model field`)
}
