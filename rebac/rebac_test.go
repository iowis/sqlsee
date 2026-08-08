package rebac

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/iowis/sqlsee"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

type resource struct {
	ID       uuid.UUID `db:"id"`
	ObjectID uuid.UUID `db:"object_id"`
	OwnerID  uuid.UUID `db:"owner_id"`
}

type recordedQuery struct {
	sql  string
	args []any
}

type recordingDB struct {
	queries []recordedQuery
	rows    []pgx.Rows
}

func (db *recordingDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.queries = append(db.queries, recordedQuery{sql: sql, args: append([]any(nil), args...)})
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
}

func newFakeRows(rows [][]any) *fakeRows {
	return &fakeRows{
		fields: []pgconn.FieldDescription{
			{Name: "id"},
			{Name: "object_id"},
			{Name: "owner_id"},
		},
		rows: rows,
	}
}

func newFakeRowsWithPermissions(rows [][]any) *fakeRows {
	return &fakeRows{
		fields: []pgconn.FieldDescription{
			{Name: "id"},
			{Name: "object_id"},
			{Name: "owner_id"},
			{Name: "permissions"},
		},
		rows: rows,
	}
}

func (r *fakeRows) Close() { r.closed = true }

func (r *fakeRows) Err() error { return nil }

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
		value := reflect.ValueOf(dest[i])
		if value.Kind() != reflect.Pointer || value.IsNil() {
			return errors.New("scan destination is not a pointer")
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

func newBase(t *testing.T, db sqlsee.DBTX, options ...sqlsee.Option) *sqlsee.Query[resource] {
	t.Helper()

	query, err := sqlsee.New[resource](db, sqlsee.Config{
		Table: "public.resources",
		Alias: "r",
	}, options...)
	require.NoError(t, err)
	return query
}

func TestListUsesInlinePermissionPredicate(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{OwnerField: "owner_id"}))
	userID := uuid.New()
	groupID := uuid.New()

	page, err := query.List(
		t.Context(),
		sqlsee.Request{},
		WithSubject(
			Subject[uuid.UUID]{UserID: userID, GroupIDs: []uuid.UUID{groupID}},
			[]int32{1, 3},
		),
	)

	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Len(t, db.queries, 1)
	require.Contains(t, db.queries[0].sql,
		`FROM "public"."resources" AS "r" LEFT JOIN LATERAL (`)
	require.Contains(t, db.queries[0].sql,
		`FROM "group_access" AS "sqlsee_rebac_access"`)
	require.Contains(t, db.queries[0].sql,
		`CROSS JOIN LATERAL unnest("sqlsee_rebac_access"."permissions") AS "sqlsee_rebac_perm"("value")`)
	require.Contains(t, db.queries[0].sql,
		`"sqlsee_rebac_access"."target_id" = "r"."id"`)
	require.Contains(t, db.queries[0].sql,
		`"sqlsee_rebac_access"."activated_at" IS NOT NULL`)
	require.Contains(t, db.queries[0].sql,
		`"r"."owner_id" = $3::uuid OR "sqlsee_rebac_permissions"."permissions" @> $4::int[]`)
	require.NotContains(t, db.queries[0].sql, "effective_group_permissions")
	require.NotContains(t, db.queries[0].sql,
		`, "sqlsee_rebac_permissions"."permissions" AS "permissions"`)
	require.Equal(t, []any{userID, []uuid.UUID{groupID}, userID, []int32{1, 3}, 51}, db.queries[0].args)
}

func TestListNormalizesNilGroups(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))

	_, err := query.List(
		t.Context(),
		sqlsee.Request{},
		WithSubject(Subject[uuid.UUID]{UserID: uuid.New()}, []int32{1}),
	)

	require.NoError(t, err)
	groups, ok := db.queries[0].args[1].([]uuid.UUID)
	require.True(t, ok)
	require.NotNil(t, groups)
	require.Empty(t, groups)
}

func TestNewSupportsCustomSchema(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{
		TargetField:            "object_id",
		OwnerField:             "owner_id",
		AccessTable:            `authorization.resource grants`,
		AccessTargetField:      "resource",
		AccessUserField:        "subject_user",
		AccessGroupField:       "subject_group",
		AccessPermissionsField: "grants",
		AccessActivatedAtField: "enabled_at",
	}))

	_, err := query.List(
		t.Context(),
		sqlsee.Request{},
		WithSubject(Subject[uuid.UUID]{UserID: uuid.New()}, []int32{2}),
	)

	require.NoError(t, err)
	sql := db.queries[0].sql
	require.Contains(t, sql, `FROM "authorization"."resource grants" AS "sqlsee_rebac_access"`)
	require.Contains(t, sql, `"sqlsee_rebac_access"."resource" = "r"."object_id"`)
	require.Contains(t, sql, `"sqlsee_rebac_access"."subject_user" = $1::uuid`)
	require.Contains(t, sql, `"sqlsee_rebac_access"."subject_group" = ANY($2::uuid[])`)
	require.Contains(t, sql, `unnest("sqlsee_rebac_access"."grants")`)
	require.Contains(t, sql, `"sqlsee_rebac_access"."enabled_at" IS NOT NULL`)
}

func TestNewValidation(t *testing.T) {
	t.Run("unknown target", func(t *testing.T) {
		query, err := sqlsee.New[resource](
			&recordingDB{},
			sqlsee.Config{Table: "resources"},
			WithReBAC[uuid.UUID](Config{TargetField: "missing"}),
		)
		require.Nil(t, query)
		require.EqualError(t, err, `sqlsee: install plugin "github.com/iowis/sqlsee/rebac": target field: sqlsee: unknown field "missing"`)
	})

	t.Run("unknown owner", func(t *testing.T) {
		query, err := sqlsee.New[resource](
			&recordingDB{},
			sqlsee.Config{Table: "resources"},
			WithReBAC[uuid.UUID](Config{OwnerField: "missing"}),
		)
		require.Nil(t, query)
		require.EqualError(t, err, `sqlsee: install plugin "github.com/iowis/sqlsee/rebac": owner field: sqlsee: unknown field "missing"`)
	})

	t.Run("invalid access table", func(t *testing.T) {
		query, err := sqlsee.New[resource](
			&recordingDB{},
			sqlsee.Config{Table: "resources"},
			WithReBAC[uuid.UUID](Config{AccessTable: "auth..grants"}),
		)
		require.Nil(t, query)
		require.EqualError(t, err,
			`sqlsee: install plugin "github.com/iowis/sqlsee/rebac": access table: sqlsee: invalid identifier "auth..grants"`)
	})

	t.Run("invalid access field", func(t *testing.T) {
		query, err := sqlsee.New[resource](
			&recordingDB{},
			sqlsee.Config{Table: "resources"},
			WithReBAC[uuid.UUID](Config{AccessTargetField: "bad\x00field"}),
		)
		require.Nil(t, query)
		require.EqualError(t, err,
			"sqlsee: install plugin \"github.com/iowis/sqlsee/rebac\": access target field: sqlsee: invalid identifier \"bad\\x00field\"")
	})
}

func TestListAcceptsNilPermissions(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))
	userID := uuid.New()

	page, err := query.List(
		t.Context(),
		sqlsee.Request{},
		WithSubject(Subject[uuid.UUID]{UserID: userID}, nil),
	)

	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Len(t, db.queries, 1)
	require.Contains(t, db.queries[0].sql,
		`"sqlsee_rebac_permissions"."permissions" <> '{}'::int[]`)
	require.NotContains(t, db.queries[0].sql, "@>")
	require.Equal(t, []any{userID, []uuid.UUID{}, 51}, db.queries[0].args)
}

func TestListAcceptsNilPermissionsWithOwner(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{OwnerField: "owner_id"}))
	userID := uuid.New()

	_, err := query.List(
		t.Context(),
		sqlsee.Request{},
		WithSubject(Subject[uuid.UUID]{UserID: userID}, nil),
	)

	require.NoError(t, err)
	require.Len(t, db.queries, 1)
	require.Contains(t, db.queries[0].sql,
		`"r"."owner_id" = $3::uuid OR "sqlsee_rebac_permissions"."permissions" <> '{}'::int[]`)
	require.NotContains(t, db.queries[0].sql, "@>")
	require.Equal(t, []any{userID, []uuid.UUID{}, userID, 51}, db.queries[0].args)
}

func TestListRequiresSubject(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))

	page, err := query.List(t.Context(), sqlsee.Request{})

	require.Empty(t, page)
	require.EqualError(t, err,
		`sqlsee: plugin "github.com/iowis/sqlsee/rebac": rebac.WithSubject is required`)
	require.Empty(t, db.queries)
}

func TestListRejectsInvalidSubjectOptionType(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))

	page, err := query.List(
		t.Context(),
		sqlsee.Request{},
		sqlsee.NewPluginOption(pluginName, "wrong type"),
	)

	require.Empty(t, page)
	require.EqualError(t, err,
		`sqlsee: plugin "github.com/iowis/sqlsee/rebac": invalid rebac.WithSubject value`)
	require.Empty(t, db.queries)
}

func TestCursorIsBoundToSubjectAndPermissions(t *testing.T) {
	firstID := uuid.New()
	secondID := uuid.New()
	ownerID := uuid.New()
	db := &recordingDB{rows: []pgx.Rows{
		newFakeRows([][]any{
			{firstID, uuid.New(), ownerID},
			{secondID, uuid.New(), ownerID},
		}),
	}}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))
	userID := uuid.New()
	subject := Subject[uuid.UUID]{UserID: userID, GroupIDs: []uuid.UUID{uuid.New()}}

	page, err := query.List(
		t.Context(),
		sqlsee.Request{Limit: 1},
		WithSubject(subject, []int32{1}),
	)
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)

	_, err = query.List(
		t.Context(),
		sqlsee.Request{Limit: 1, Cursor: page.NextCursor},
		WithSubject(
			Subject[uuid.UUID]{UserID: uuid.New(), GroupIDs: subject.GroupIDs},
			[]int32{1},
		),
	)
	require.EqualError(t, err, "sqlsee: cursor does not belong to this query")

	_, err = query.List(
		t.Context(),
		sqlsee.Request{Limit: 1, Cursor: page.NextCursor},
		WithSubject(subject, []int32{2}),
	)
	require.EqualError(t, err, "sqlsee: cursor does not belong to this query")
	require.Len(t, db.queries, 1)
}

func TestListProjectsPermissionsColumn(t *testing.T) {
	db := &recordingDB{}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))
	userID := uuid.New()

	_, err := List[resource, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{},
		Subject[uuid.UUID]{UserID: userID},
		[]int32{1},
	)

	require.NoError(t, err)
	require.Len(t, db.queries, 1)
	require.Contains(t, db.queries[0].sql, `AS "permissions"`)
	require.Contains(t, db.queries[0].sql,
		`"sqlsee_rebac_permissions"."permissions" AS "permissions"`)
	require.Equal(t, []any{userID, []uuid.UUID{}, []int32{1}, 51}, db.queries[0].args)
}

func TestListReturnsEffectivePermissions(t *testing.T) {
	firstID := uuid.New()
	secondID := uuid.New()
	ownerID := uuid.New()
	db := &recordingDB{rows: []pgx.Rows{
		newFakeRowsWithPermissions([][]any{
			{firstID, uuid.New(), ownerID, []any{int32(1), int32(2), int32(4)}},
			{secondID, uuid.New(), ownerID, []any{int32(3)}},
		}),
	}}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))
	userID := uuid.New()

	page, err := List[resource, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{},
		Subject[uuid.UUID]{UserID: userID},
		[]int32{1},
	)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, []int32{1, 2, 4}, page.Items[0].Permissions)
	require.Equal(t, firstID, page.Items[0].Model.ID)
	require.Equal(t, []int32{3}, page.Items[1].Permissions)
	require.Equal(t, secondID, page.Items[1].Model.ID)
}

func TestListWithNilPermissionsProjectsAllPermissions(t *testing.T) {
	firstID := uuid.New()
	secondID := uuid.New()
	ownerID := uuid.New()
	db := &recordingDB{rows: []pgx.Rows{
		newFakeRowsWithPermissions([][]any{
			{firstID, uuid.New(), ownerID, []any{int32(1), int32(2), int32(4)}},
			{secondID, uuid.New(), ownerID, []any{int32(3)}},
		}),
	}}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))
	userID := uuid.New()

	page, err := List[resource, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{},
		Subject[uuid.UUID]{UserID: userID},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, []int32{1, 2, 4}, page.Items[0].Permissions)
	require.Equal(t, firstID, page.Items[0].Model.ID)
	require.Equal(t, []int32{3}, page.Items[1].Permissions)
	require.Equal(t, secondID, page.Items[1].Model.ID)
	require.Contains(t, db.queries[0].sql, `<> '{}'::int[]`)
	require.NotContains(t, db.queries[0].sql, "@>")
	require.Equal(t, []any{userID, []uuid.UUID{}, 51}, db.queries[0].args)
}

func TestListConvertsAnySlicePermissions(t *testing.T) {
	db := &recordingDB{rows: []pgx.Rows{
		newFakeRowsWithPermissions([][]any{
			{uuid.New(), uuid.New(), uuid.New(), []any{int32(100), int32(101)}},
		}),
	}}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))

	page, err := List[resource, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{},
		Subject[uuid.UUID]{UserID: uuid.New()},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, []int32{100, 101}, page.Items[0].Permissions)
}

func TestListProjectionCursorBoundToSubjectAndPermissions(t *testing.T) {
	firstID := uuid.New()
	ownerID := uuid.New()
	db := &recordingDB{rows: []pgx.Rows{
		newFakeRowsWithPermissions([][]any{
			{firstID, uuid.New(), ownerID, []int32{1}},
			{uuid.New(), uuid.New(), ownerID, []int32{1}},
		}),
	}}
	query := newBase(t, db, WithReBAC[uuid.UUID](Config{}))
	userID := uuid.New()
	subject := Subject[uuid.UUID]{UserID: userID, GroupIDs: []uuid.UUID{uuid.New()}}

	page, err := List[resource, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{Limit: 1},
		subject,
		[]int32{1},
	)
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)

	_, err = List[resource, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{Limit: 1, Cursor: page.NextCursor},
		Subject[uuid.UUID]{UserID: uuid.New(), GroupIDs: subject.GroupIDs},
		[]int32{1},
	)
	require.EqualError(t, err, "sqlsee: cursor does not belong to this query")

	_, err = List[resource, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{Limit: 1, Cursor: page.NextCursor},
		subject,
		[]int32{2},
	)
	require.EqualError(t, err, "sqlsee: cursor does not belong to this query")
}

func TestListProjectionRejectsAliasCollision(t *testing.T) {
	type modelWithPermissions struct {
		ID          uuid.UUID `db:"id"`
		Permissions []int32   `db:"permissions"`
	}

	db := &recordingDB{}
	query, err := sqlsee.New[modelWithPermissions](db, sqlsee.Config{
		Table: "resources",
	}, WithReBAC[uuid.UUID](Config{}))
	require.NoError(t, err)

	page, err := List[modelWithPermissions, uuid.UUID](
		t.Context(),
		query,
		sqlsee.Request{},
		Subject[uuid.UUID]{UserID: uuid.New()},
		[]int32{1},
	)

	require.Empty(t, page.Items)
	require.EqualError(t, err,
		`sqlsee: plugin select alias "permissions" collides with model field`)
}
