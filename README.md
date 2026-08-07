# iowis/sqlsee
`sqlsee` is a small, type-safe list-query helper for PostgreSQL and
[`pgx`](https://github.com/jackc/pgx). It builds allowlisted filters, stable
sorting, and cursor-based pagination, then scans the result into a Go struct.
It exists because we needed dynamic queries that work on top of sqlc models.

It is useful for list endpoints that need:

- keyset pagination instead of offset pagination;
- client-selectable filters and sort fields;
- signed cursors that cannot be modified by clients; and
- compatibility with `pgxpool.Pool`, `pgx.Conn`, and `pgx.Tx`.

## Requirements

- Go 1.26 or newer
- PostgreSQL
- `github.com/jackc/pgx/v5`

## Installation

```sh
go get github.com/iowis/sqlsee
```

## Quick start

The result type must be a struct with exported fields and `db` tags matching
the PostgreSQL column names:

```go
package groups

import (
	"context"
	"os"
	"time"

	"github.com/iowis/sqlsee"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Group struct {
	ID          int64     `db:"id" json:"id"`
	Name        string    `db:"name" json:"name"`
	Description string    `db:"description" json:"description"`
	OwnerID     int64     `db:"owner_id" json:"ownerId"`
	CreatedAt   time.Time `db:"created_at" json:"createdAt"`
}

func Example(ctx context.Context) error {
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()

	groups, err := sqlsee.New[Group](pool, sqlsee.Config{
		Table:      "public.groups",
		Alias:      "g",
		PrimaryKey: "id",

		// Only these fields may be supplied by clients as filters or sorts.
		Filterable: []string{"name", "description"},
		Sortable:   []string{"id", "name", "created_at"},

		DefaultSort:  []sqlsee.Sort{sqlsee.Desc("created_at")},
		DefaultLimit: 50,
		MaxLimit:     100,

		// Store a long random value in an environment variable.
		CursorSecret: []byte(os.Getenv("SQLSEE_CURSOR_SECRET")),
	})
	if err != nil {
		return err
	}

	page, err := groups.List(ctx, sqlsee.Request{
		Limit: 25,
		Where: []sqlsee.FilterGroup{
			sqlsee.Any(
				sqlsee.ILike("name", "%platform%"),
				sqlsee.ILike("description", "%platform%"),
			),
		},
		Sort: []sqlsee.Sort{
			sqlsee.Asc("name"),
		},
	})
	if err != nil {
		return err
	}

	_ = page.Items
	_ = page.HasMore
	_ = page.NextCursor
	return nil
}
```

Create a `Query` once and reuse it. `New` validates the model and configuration;
`List` is safe to call concurrently as long as the supplied database handle is
safe for concurrent use.

## Fetching the next page

Return `NextCursor` to the client when `HasMore` is true. On the next request,
send the cursor back with the same filters and sorting:

```go
nextPage, err := groups.List(ctx, sqlsee.Request{
	Limit:  25,
	Cursor: page.NextCursor,
	Where: []sqlsee.FilterGroup{
		sqlsee.Any(
			sqlsee.ILike("name", "%platform%"),
			sqlsee.ILike("description", "%platform%"),
		),
	},
	Sort: []sqlsee.Sort{
		sqlsee.Asc("name"),
	},
})
```

A cursor is tied to its table, filters, and resolved sorting. Changing any of
those values causes `List` to reject it. The page limit may be changed between
requests, provided it remains within the configured maximum.

## Filters

`sqlsee` supports PostgreSQL `ILIKE`, `LIKE`, and equality filters. Patterns use
PostgreSQL syntax: `%` matches any sequence and `_` matches one character.

```go
Where: []sqlsee.FilterGroup{
	// name ILIKE '%team%' AND description ILIKE '%active%'
	sqlsee.All(
		sqlsee.ILike("name", "%team%"),
		sqlsee.ILike("description", "%active%"),
	),

	// AND name LIKE 'Platform%' AND description = 'Internal'
	sqlsee.All(
		sqlsee.Like("name", "Platform%"),
		sqlsee.Equals("description", "Internal"),
	),

	// AND (name ILIKE 'ops%' OR description ILIKE '%operations%')
	sqlsee.Any(
		sqlsee.ILike("name", "ops%"),
		sqlsee.ILike("description", "%operations%"),
	),
}
```

Filters inside `All` are joined with `AND`; filters inside `Any` are joined with
`OR`. Separate groups are always joined with `AND`. Empty groups are ignored.
Using a field that is not in `Config.Filterable` returns an error.

`ILike` and `Like` accept string patterns. `Equals` accepts any value supported
by pgx, such as strings, booleans, numbers, UUIDs, and timestamps. All filter
values are passed as query parameters. Field, table, and alias names are quoted
identifiers, and filter and sort fields must come from the validated
configuration. `Equals(field, nil)` generates `IS NULL`.

## Plugins and custom SQL

Plugins extend the generated base query while `*sqlsee.Query[T]` remains the
type applications use. Install plugins when constructing a query and pass their
typed options to `List`:

```go
groups, err := sqlsee.New[Group](pool, config,
	tenant.WithTenantScope(),
)

page, err := groups.List(ctx, request,
	tenant.WithTenant(tenantID),
)
```

A plugin implements `sqlsee.Plugin`. Its installation step receives validated
model metadata and returns a handler. The handler can add parameterized joins
and predicates for each request:

```go
const pluginName = "example.com/tenant-scope"

type plugin struct{}

func (plugin) Name() string { return pluginName }

func (plugin) Install(ctx sqlsee.PluginContext) (sqlsee.PluginHandler, error) {
	id, err := ctx.Column("id")
	if err != nil {
		return nil, err
	}

	return func(
		_ context.Context,
		input sqlsee.PluginInput,
		builder *sqlsee.PluginBuilder,
	) error {
		if !input.Present() {
			return errors.New("tenant is required")
		}

		if err := builder.Join(
			`JOIN memberships AS m ON m.group_id = `+id,
		); err != nil {
			return err
		}
		return builder.Where(`m.tenant_id = $1`, input.Value())
	}, nil
}

func WithTenantScope() sqlsee.Option {
	return sqlsee.WithPlugin(plugin{})
}

func WithTenant(tenantID uuid.UUID) sqlsee.ListOption {
	return sqlsee.NewPluginOption(pluginName, tenantID)
}
```

`PluginBuilder.Join` accepts a complete PostgreSQL join clause.
`PluginBuilder.Where` accepts a predicate without the `WHERE` keyword; sqlsee
combines it with other predicates using `AND`. Placeholders are local to each
clause and start at `$1`. sqlsee validates and renumbers them in the final query,
so values remain pgx parameters. SQL text must come from trusted plugin code,
never from a client.

Every installed plugin runs once per `List` call in installation order. A
handler receives `PluginInput.Present() == false` when its per-call option was
omitted, allowing security plugins to fail closed. Unknown and duplicate
per-call plugin options are rejected.

Plugin clauses, arguments, and installation order are part of the cursor
fingerprint. Supply the same plugin options when fetching the next page; a
cursor cannot be reused with a different plugin scope.

## ReBAC extension

The `rebac` subpackage is a bundled plugin for relationship-based authorization.
It does not require a PostgreSQL permission function: it injects the permission
lookup directly as a correlated predicate while preserving the normal Query API:

```go
groups, err := sqlsee.New[Group](pool, sqlsee.Config{
	Table: "public.groups",
	Alias: "g",
}, rebac.WithReBAC[uuid.UUID](rebac.Config{
	OwnerField: "owner_id",

	// These names are defaults and can be changed independently.
	AccessTable:            "group_access",
	AccessTargetField:      "target_id",
	AccessUserField:        "user_id",
	AccessGroupField:       "group_id",
	AccessPermissionsField: "permissions",
	AccessActivatedAtField: "activated_at",
}))
if err != nil {
	return err
}

page, err := groups.List(
	ctx,
	sqlsee.Request{Limit: 25},
	rebac.WithSubject(
		rebac.Subject[uuid.UUID]{
			UserID:   userID,
			GroupIDs: groupIDs,
		},
		[]int32{permissionRead, permissionList},
	),
)
```

`TargetField` defaults to the base query's primary key. An access row contributes
permissions only when its configured activation column is non-null and its user
or group matches the subject. The combined permissions must contain every
requested permission. When `OwnerField` is set, ownership grants access without
requiring an access row.

The access table may be schema-qualified, and every table and column identifier
is safely quoted. The extension expects UUID subjects and targets plus PostgreSQL
`int[]` permissions. Nil group slices are sent as empty arrays; an empty
permission request is rejected. Omitting `rebac.WithSubject` is also rejected,
so an installed ReBAC plugin fails closed. Subject, groups, and permissions are
included in the cursor scope automatically.

## Sorting

Use `sqlsee.Asc` and `sqlsee.Desc`:

```go
Sort: []sqlsee.Sort{
	sqlsee.Asc("name"),
	sqlsee.Desc("created_at"),
}
```

A request may specify at most two sort fields. If no sort is supplied,
`Config.DefaultSort` is used; if no default is configured, the primary key is
sorted ascending.

For stable keyset pagination, `sqlsee` automatically adds the primary key as a
final tie-breaker when it is not already present. The tie-breaker uses the same
direction as the last requested sort field.

Sort columns used to create a next-page cursor must be non-null. Prefer
non-nullable database columns for all configured sort fields.

## Configuration

| Field | Description |
| --- | --- |
| `Table` | Required table name. Schema-qualified names such as `public.groups` are supported. |
| `Alias` | SQL table alias. Defaults to `t`. |
| `PrimaryKey` | Unique, non-null model column used as the pagination tie-breaker. Defaults to `id`. |
| `Filterable` | Model columns clients are allowed to filter with `ILike`, `Like`, or `Equals`. |
| `Sortable` | Model columns clients are allowed to sort. The primary key is always allowed. |
| `DefaultSort` | Sort used when a request does not specify one. Supports at most two fields. Defaults to primary key ascending. |
| `DefaultLimit` | Page size used when `Request.Limit` is zero. Defaults to `50`. |
| `MaxLimit` | Largest accepted page size. Defaults to `100`. |
| `CursorSecret` | Optional HMAC-SHA256 key used to sign cursors. Strongly recommended for client-facing APIs. |

`Table`, `PrimaryKey`, `Filterable`, `Sortable`, and sort values refer to
database column names from `db` tags, not Go field names.

## Request and response

```go
type Request struct {
	Limit  int
	Cursor string
	Where  []sqlsee.FilterGroup
	Sort   []sqlsee.Sort
}

type Page[T any] struct {
	Items      []T
	NextCursor string
	HasMore    bool
}
```

`Limit: 0` selects the configured default. Other limits must be between `1` and
`MaxLimit`. `NextCursor` is populated only when `HasMore` is true.

## Using sqlc models

`sqlsee` works with generated sqlc structs when database tags are enabled:

```yaml
version: "2"
sql:
  - engine: postgresql
    gen:
      go:
        emit_db_tags: true
```

Only exported fields with a non-empty `db` tag are selected. Fields without a
tag, fields tagged `db:"-"`, and unexported fields are ignored. Duplicate tags
are rejected.

## Column helper

`ColumnsFor` returns a safely quoted select list for a tagged model. This is
useful when composing a custom query that must remain compatible with pgx's
name-based struct scanning:

```go
columns, err := sqlsee.ColumnsFor[Group]("g")
// []string{
//     `"g"."id" AS "id"`,
//     `"g"."name" AS "name"`,
//     ...
// }
```

Unlike `New`, `ColumnsFor` also accepts pointer model types such as
`ColumnsFor[*Group]("g")`.

For plugins that add trusted SQL, `PluginContext.Column` validates a model field
and returns its safely quoted, alias-qualified expression.
`PluginContext.PrimaryKey`, `QuoteIdentifier`, and `QuoteQualifiedIdentifier`
provide the remaining read-only metadata and identifier handling needed to
build reusable clauses.

## Cursor security

With `CursorSecret` configured, cursors are authenticated with HMAC-SHA256 and
modified cursors are rejected. Use the same secret across all application
instances and keep it stable across deployments while issued cursors remain
valid.

Without a secret, cursors are only URL-safe base64 encoded. They are not
encrypted, and clients can read or modify their contents. Do not place sensitive
values in sortable columns, and configure a secret for untrusted clients.

## Current scope

`sqlsee` intentionally focuses on a narrow list-query API:

- PostgreSQL and pgx only;
- `ILIKE`, `LIKE`, and equality filters;
- up to two client-specified sort fields;
- one base table with optional plugin-provided trusted joins and predicates; and
- forward cursor pagination only.

## License

This project is licensed under the MIT License. See [LICENSE.md](./LICENSE.md) for details.
