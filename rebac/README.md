# rebac

The `rebac` subpackage is a bundled plugin for `github.com/iowis/sqlsee` that
provides relationship-based authorization. It does not require a PostgreSQL
permission function: it injects the permission lookup directly as a
`LEFT JOIN LATERAL` correlated subquery while preserving the normal `Query` API.

## Installation

`rebac` is part of the `sqlsee` module; import it alongside the core package:

```go
import (
	"github.com/iowis/sqlsee"
	"github.com/iowis/sqlsee/rebac"
)
```

## Quick start

Install the plugin when constructing a `Query` and supply a subject plus the
required permissions on each `List` call:

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
or group matches the subject. When `permissions` is non-empty, the combined
permissions must contain every requested permission. When `permissions` is nil,
every row the subject can access (at least one granted permission, or via
ownership when configured) is returned. When `OwnerField` is set, ownership
grants access without requiring an access row.

The access table may be schema-qualified, and every table and column identifier
is safely quoted. The extension expects UUID subjects and targets plus PostgreSQL
`int[]` permissions. Nil group slices are sent as empty arrays. Omitting
`rebac.WithSubject` is rejected, so an installed ReBAC plugin fails closed.
Subject, groups, and permissions are included in the cursor scope automatically.

## Projecting effective permissions

`WithSubject` filters rows without returning the effective set. To also surface
the permissions the subject holds on each item, use the `rebac.List` helper,
which returns a `Page[rebac.Item[T]]`:

```go
page, err := rebac.List[Group, uuid.UUID](
	ctx,
	groups,
	sqlsee.Request{Limit: 25},
	rebac.Subject[uuid.UUID]{
		UserID:   userID,
		GroupIDs: groupIDs,
	},
	[]int32{permissionRead, permissionList},
)
```

Each item wraps the model and the effective permissions:

```go
type Item[T any] struct {
	Model       T       `json:"model"`
	Permissions []int32 `json:"permissions"`
}
```

The `Permissions` field always contains the full set of distinct permissions the
subject has on that item, regardless of whether the optional `permissions`
filter was supplied. The projection is opt-in: plain `Query.List` with
`WithSubject` filters without computing the projection. `rebac.List` uses
`Query.ListWithExtras` internally to collect the projected column. The
projection alias defaults to `permissions`; the plugin rejects the call if a
model already has a `permissions` column.

## How the permission lookup works

The plugin emits a single `LEFT JOIN LATERAL` that computes, for each base row,
the sorted, deduplicated union of permissions granted to the subject via
activated access rows. The same computed array is reused both to filter rows
(`array @> $N::int[]`) and, when projecting, to populate the `permission`
column. Schematically:

```sql
SELECT ..., "sqlsee_rebac_permissions"."permissions" AS "permissions"
FROM <table> AS <alias>
LEFT JOIN LATERAL (
  SELECT COALESCE(
    array_agg(DISTINCT p.value ORDER BY p.value),
    '{}'::int[]
  ) AS "permissions"
  FROM <access_table> AS "sqlsee_rebac_access"
  CROSS JOIN LATERAL unnest("sqlsee_rebac_access"."permissions")
      AS "sqlsee_rebac_perm"("value") p
  WHERE "sqlsee_rebac_access"."<target>" = <alias>."<target_field>"
    AND "sqlsee_rebac_access"."<activated_at>" IS NOT NULL
    AND (
      "sqlsee_rebac_access"."<user_id>"  = $1::uuid
      OR "sqlsee_rebac_access"."<group_id>" = ANY($2::uuid[])
    )
) AS "sqlsee_rebac_permissions" ON true
WHERE (
  <alias>."<owner_field>" = $3::uuid               -- only when OwnerField is set
  OR "sqlsee_rebac_permissions"."permissions" @> $4::int[]
)
```

When `permissions` is nil, the `@>` predicate is replaced by
`"sqlsee_rebac_permissions"."permissions" <> '{}'::int[]` (and the
corresponding bind argument is omitted), so any row with at least one granted
permission qualifies.

Because the join is a `LEFT JOIN LATERAL ... ON true`, it never drops base rows;
rows without matching access rows simply receive an empty `int[]` and are then
filtered out by the predicate unless ownership grants access.

## Configuration

`Config` maps a listed model and an access table to the ReBAC predicate:

| Field | Description | Default |
| --- | --- | --- |
| `TargetField` | Listed model column matched to `AccessTargetField`. | base query's primary key |
| `OwnerField` | Optional column granting access when it equals `Subject.UserID`. | none |
| `AccessTable` | Access table name; may be schema-qualified. | `group_access` |
| `AccessTargetField` | Access-table column matching the listed row. | `target_id` |
| `AccessUserField` | Access-table column matching the subject's user id. | `user_id` |
| `AccessGroupField` | Access-table column matching one of the subject's group ids. | `group_id` |
| `AccessPermissionsField` | Access-table `int[]` column containing granted permissions. | `permissions` |
| `AccessActivatedAtField` | Access-table column that must be non-null for a row to contribute. | `activated_at` |

All identifiers are safely quoted via `sqlsee.QuoteIdentifier` /
`sqlsee.QuoteQualifiedIdentifier`.

## API reference

- `WithReBAC[ID any](cfg Config) sqlsee.Option` — installs the plugin on a
  `Query`.
- `WithSubject[ID any](subject Subject[ID], permissions []int32) sqlsee.ListOption`
  — per-call option that filters rows. `permissions` is optional: when non-empty
  only rows whose effective permissions contain every value are returned; when
  nil, every row the subject can access is returned.
- `List[T any, ID any](ctx, q, req, subject, permissions) (sqlsee.Page[Item[T]], error)`
  — per-call helper that also projects the effective permissions per item.
  `permissions` is optional and acts only as a filter; `Item.Permissions` always
  contains the full set of permissions the subject has on the item.
- `Subject[ID any]{ UserID ID; GroupIDs []ID }` — identifies the caller.
- `Item[T any]{ Model T; Permissions []int32 }` — wrapper returned by `List`.
