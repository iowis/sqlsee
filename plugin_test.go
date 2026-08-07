package sqlsee

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func testWhere(sql string, args ...any) sqlClause {
	return sqlClause{kind: sqlClauseWhere, sql: sql, args: args}
}

func testJoin(sql string, args ...any) sqlClause {
	return sqlClause{kind: sqlClauseJoin, sql: sql, args: args}
}

func testSelect(sql, alias string, args ...any) sqlClause {
	return sqlClause{kind: sqlClauseSelect, sql: sql, alias: alias, args: args}
}

func TestCompileSQLClauseRenumbersPostgreSQLPlaceholders(t *testing.T) {
	clause := testWhere(
		`value = $1 OR backup = $1 OR other = $2`,
		"primary",
		"secondary",
	)

	compiled, err := compileSQLClause(clause, 3)

	require.NoError(t, err)
	require.Equal(t, `value = $4 OR backup = $4 OR other = $5`, compiled)
}

func TestCompileSQLClauseIgnoresQuotedAndCommentedPlaceholders(t *testing.T) {
	clause := testWhere(`
		actual = $1
		AND single_quoted = '$2;'
		AND "quoted$3" = $$body $4;$$
		AND tagged = $tag$body $5;$tag$
		-- ignored $6;
		/* ignored $7; /* nested $8 */ */
	`, 42)

	compiled, err := compileSQLClause(clause, 2)

	require.NoError(t, err)
	require.Contains(t, compiled, `actual = $3`)
	require.Contains(t, compiled, `single_quoted = '$2;'`)
	require.Contains(t, compiled, `"quoted$3" = $$body $4;$$`)
	require.Contains(t, compiled, `$tag$body $5;$tag$`)
	require.Contains(t, compiled, `-- ignored $6;`)
	require.Contains(t, compiled, `/* ignored $7; /* nested $8 */ */`)
}

func TestCompileSQLClauseIgnoresDollarSignsInIdentifiers(t *testing.T) {
	compiled, err := compileSQLClause(testWhere(`column$1 = 1`), 2)

	require.NoError(t, err)
	require.Equal(t, `column$1 = 1`, compiled)
}

func TestCompileSQLClauseTerminatesTrailingLineComment(t *testing.T) {
	compiled, err := compileSQLClause(
		testWhere(`active = $1 -- ignored $2;`, true),
		0,
	)

	require.NoError(t, err)
	require.Equal(t, "active = $1 -- ignored $2;\n", compiled)
}

func TestCompileSQLClauseValidation(t *testing.T) {
	tests := []struct {
		name   string
		clause sqlClause
		err    string
	}{
		{
			name:   "zero value",
			clause: sqlClause{},
			err:    "sqlsee: invalid custom SQL clause",
		},
		{
			name:   "empty",
			clause: testWhere("  "),
			err:    "sqlsee: custom SQL clause must not be empty",
		},
		{
			name:   "where keyword",
			clause: testWhere("WHERE(active)"),
			err:    "sqlsee: plugin Where must not include the WHERE keyword",
		},
		{
			name:   "invalid join",
			clause: testJoin("memberships AS m"),
			err:    "sqlsee: plugin Join must contain a complete JOIN clause",
		},
		{
			name:   "zero placeholder",
			clause: testWhere("value = $0", 1),
			err:    "sqlsee: custom SQL placeholders must start at $1",
		},
		{
			name:   "missing argument",
			clause: testWhere("value = $2", 1),
			err:    "sqlsee: custom SQL placeholder $2 has no argument",
		},
		{
			name:   "unused argument",
			clause: testWhere("value = $1", 1, 2),
			err:    "sqlsee: custom SQL argument 2 has no placeholder",
		},
		{
			name:   "semicolon",
			clause: testWhere("active = true; DROP TABLE users"),
			err:    "sqlsee: custom SQL clauses must not contain semicolons",
		},
		{
			name:   "unterminated quote",
			clause: testWhere("value = 'open"),
			err:    "sqlsee: unterminated quoted value in custom SQL",
		},
		{
			name:   "unterminated comment",
			clause: testWhere("value = true /* open"),
			err:    "sqlsee: unterminated block comment in custom SQL",
		},
		{
			name:   "unterminated dollar quote",
			clause: testWhere("value = $tag$open"),
			err:    "sqlsee: unterminated dollar-quoted string in custom SQL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compiled, err := compileSQLClause(tt.clause, 0)
			require.Empty(t, compiled)
			require.EqualError(t, err, tt.err)
		})
	}
}

func TestJoinAcceptsPostgreSQLJoinForms(t *testing.T) {
	joins := []sqlClause{
		testJoin("JOIN memberships AS m ON true"),
		testJoin("LEFT OUTER\nJOIN memberships AS m ON true"),
		testJoin("NATURAL FULL JOIN memberships"),
	}

	for _, join := range joins {
		_, err := compileSQLClause(join, 0)
		require.NoError(t, err)
	}
}

func TestCompileSQLClauseRenumbersSelectPlaceholders(t *testing.T) {
	clause := testSelect(
		`COALESCE(array_agg($1), '{}'::int[])`,
		"permission",
		[]int32{1, 2},
	)

	compiled, err := compileSQLClause(clause, 5)

	require.NoError(t, err)
	require.Equal(t, `COALESCE(array_agg($6), '{}'::int[])`, compiled)
}

func TestPluginBuilderSelectValidation(t *testing.T) {
	t.Run("renumbers placeholders", func(t *testing.T) {
		var b PluginBuilder
		require.NoError(t, b.Select(`$1::int[]`, "permission", []int32{1}))
		require.Len(t, b.clauses, 1)
	})

	t.Run("rejects empty alias", func(t *testing.T) {
		var b PluginBuilder
		err := b.Select(`1`, "", 1)
		require.EqualError(t, err, `sqlsee: select alias: sqlsee: invalid identifier ""`)
	})

	t.Run("rejects invalid alias", func(t *testing.T) {
		var b PluginBuilder
		err := b.Select(`1`, "bad\x00alias", 1)
		require.ErrorContains(t, err, "sqlsee: select alias")
	})

	t.Run("rejects empty sql", func(t *testing.T) {
		var b PluginBuilder
		err := b.Select(``, "permission", 1)
		require.EqualError(t, err, "sqlsee: custom SQL clause must not be empty")
	})

	t.Run("rejects duplicate alias", func(t *testing.T) {
		var b PluginBuilder
		require.NoError(t, b.Select(`$1::int`, "permission", 1))
		err := b.Select(`$1::int`, "permission", 2)
		require.EqualError(t, err, `sqlsee: duplicate select alias "permission"`)
	})

	t.Run("rejects semicolons", func(t *testing.T) {
		var b PluginBuilder
		err := b.Select(`$1; DROP TABLE`, "permission", 1)
		require.EqualError(t, err, "sqlsee: custom SQL clauses must not contain semicolons")
	})

	t.Run("rejects unused arguments", func(t *testing.T) {
		var b PluginBuilder
		err := b.Select(`$1`, "permission", 1, 2)
		require.EqualError(t, err, "sqlsee: custom SQL argument 2 has no placeholder")
	})
}

func TestApplyPluginResultsCollectsSelectColumns(t *testing.T) {
	results := []pluginResult{
		{
			name: "test",
			clauses: []sqlClause{
				testSelect(`$1::int[]`, "permission", []int32{1}),
				testWhere(`"perm" @> $1`, []int32{1}),
			},
		},
	}

	b := &sqlBuilder{}
	joins, selects, _, err := applyPluginResults(b, results)
	require.NoError(t, err)
	require.Empty(t, joins)
	require.Len(t, selects, 1)
	require.Equal(t, "permission", selects[0].alias)
	require.Equal(t, `$1::int[]`, selects[0].sql)
	require.Equal(t, []any{[]int32{1}, []int32{1}}, b.args)
}
