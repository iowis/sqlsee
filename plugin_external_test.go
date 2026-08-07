package sqlsee_test

import (
	"context"
	"errors"
	"testing"

	"github.com/iowis/sqlsee"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

const tenantPluginName = "example.com/tenant-scope"

var errQueryStopped = errors.New("query stopped")

type externalModel struct {
	ID int `db:"id"`
}

type captureDB struct {
	sql  string
	args []any
}

func (db *captureDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.sql = sql
	db.args = append([]any(nil), args...)
	return nil, errQueryStopped
}

type tenantPlugin struct{}

func (tenantPlugin) Name() string { return tenantPluginName }

func (tenantPlugin) Install(ctx sqlsee.PluginContext) (sqlsee.PluginHandler, error) {
	id, err := ctx.Column("id")
	if err != nil {
		return nil, err
	}

	return func(_ context.Context, input sqlsee.PluginInput, builder *sqlsee.PluginBuilder) error {
		if !input.Present() {
			return errors.New("tenant is required")
		}
		return builder.Where(id+" = $1", input.Value())
	}, nil
}

func withTenant(tenantID int) sqlsee.ListOption {
	return sqlsee.NewPluginOption(tenantPluginName, tenantID)
}

func TestThirdPartyPluginUsesOnlyPublicAPI(t *testing.T) {
	db := &captureDB{}
	query, err := sqlsee.New[externalModel](
		db,
		sqlsee.Config{Table: "resources"},
		sqlsee.WithPlugin(tenantPlugin{}),
	)
	require.NoError(t, err)

	_, err = query.List(t.Context(), sqlsee.Request{}, withTenant(42))

	require.ErrorIs(t, err, errQueryStopped)
	require.Equal(t,
		`SELECT "t"."id" AS "id" FROM "resources" AS "t" WHERE ("t"."id" = $1) ORDER BY "t"."id" ASC LIMIT $2`,
		db.sql,
	)
	require.Equal(t, []any{42, 51}, db.args)
}
