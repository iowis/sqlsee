package sqlsee

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

type testStruct struct {
	ID          uuid.UUID          `db:"id"`
	Name        string             `db:"name"`
	OwnerID     uuid.UUID          `db:"owner_id"`
	IsSystem    bool               `db:"is_system"`
	Description pgtype.Text        `db:"description"`
	CreatedAt   pgtype.Timestamptz `db:"created_at"`
	UpdatedAt   pgtype.Timestamptz `db:"updated_at"`
}

func TestColumnsFor(t *testing.T) {
	t.Run("retrieves columns", func(t *testing.T) {
		cols, err := ColumnsFor[testStruct]("g")

		require.NoError(t, err)
		require.Equal(
			t,
			strings.Join([]string{
				`"g"."id" AS "id"`,
				`"g"."name" AS "name"`,
				`"g"."owner_id" AS "owner_id"`,
				`"g"."is_system" AS "is_system"`,
				`"g"."description" AS "description"`,
				`"g"."created_at" AS "created_at"`,
				`"g"."updated_at" AS "updated_at"`,
			}, " "),
			strings.Join(cols, " "),
		)
	})

	t.Run("supports pointer types", func(t *testing.T) {
		cols, err := ColumnsFor[*testStruct]("g")

		require.NoError(t, err)
		require.Equal(
			t,
			[]string{
				`"g"."id" AS "id"`,
				`"g"."name" AS "name"`,
				`"g"."owner_id" AS "owner_id"`,
				`"g"."is_system" AS "is_system"`,
				`"g"."description" AS "description"`,
				`"g"."created_at" AS "created_at"`,
				`"g"."updated_at" AS "updated_at"`,
			},
			cols,
		)
	})

	t.Run("supports nested pointer types", func(t *testing.T) {
		cols, err := ColumnsFor[**testStruct]("g")

		require.NoError(t, err)
		require.Len(t, cols, 7)
	})

	t.Run("skips fields without db tags", func(t *testing.T) {
		type model struct {
			ID   int `db:"id"`
			Name string
		}

		cols, err := ColumnsFor[model]("m")

		require.NoError(t, err)
		require.Equal(t, []string{`"m"."id" AS "id"`}, cols)
	})

	t.Run("skips fields explicitly ignored by db tag", func(t *testing.T) {
		type model struct {
			ID        int    `db:"id"`
			Transient string `db:"-"`
		}

		cols, err := ColumnsFor[model]("m")

		require.NoError(t, err)
		require.Equal(t, []string{`"m"."id" AS "id"`}, cols)
	})

	t.Run("skips unexported fields", func(t *testing.T) {
		type model struct {
			ID     int    `db:"id"`
			secret string `db:"secret"`
		}

		cols, err := ColumnsFor[model]("m")

		require.NoError(t, err)
		require.Equal(t, []string{`"m"."id" AS "id"`}, cols)
	})

	t.Run("quotes table aliases and column names", func(t *testing.T) {
		type model struct {
			Value string `db:"value\"quoted"`
		}

		cols, err := ColumnsFor[model](`table"alias`)

		require.NoError(t, err)
		require.Equal(t, []string{
			`"table""alias"."value""quoted" AS "value""quoted"`,
		}, cols)
	})

	t.Run("returns error for non-struct type", func(t *testing.T) {
		cols, err := ColumnsFor[string]("g")

		require.Nil(t, cols)
		require.EqualError(t, err, "sqlsee: string is not a struct")
	})

	t.Run("returns error for pointer to non-struct type", func(t *testing.T) {
		cols, err := ColumnsFor[*string]("g")

		require.Nil(t, cols)
		require.EqualError(t, err, "sqlsee: string is not a struct")
	})

	t.Run("returns error when struct has no db-tagged fields", func(t *testing.T) {
		type model struct {
			Name      string
			Transient string `db:"-"`
			private   string `db:"private"`
		}

		cols, err := ColumnsFor[model]("m")

		require.Nil(t, cols)
		require.EqualError(
			t,
			err,
			"sqlsee: sqlsee.model has no db tags; enable sqlc emit_db_tags",
		)
	})

	t.Run("column cache", func(t *testing.T) {
		groupColumns, err := ColumnsFor[testStruct]("g")
		require.NoError(t, err)

		userColumns, err := ColumnsFor[testStruct]("u")
		require.NoError(t, err)

		require.Equal(t, `"g"."id" AS "id"`, groupColumns[0])
		require.Equal(t, `"u"."id" AS "id"`, userColumns[0])
	})
}
