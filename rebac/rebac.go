// Package rebac restricts sqlsee list queries using relationship-based access
// control stored in a PostgreSQL access table.
package rebac

import (
	"context"
	"fmt"

	"github.com/iowis/sqlsee"
)

const pluginName = "github.com/iowis/sqlsee/rebac"

// Subject identifies the user and groups used to resolve effective permissions.
type Subject[ID any] struct {
	UserID   ID
	GroupIDs []ID
}

// Config maps a listed model and an access table to the ReBAC predicate.
type Config struct {
	// TargetField is the listed model column matched to AccessTargetField.
	// It defaults to the base query's primary key.
	TargetField string
	// OwnerField optionally grants access when it equals Subject.UserID.
	OwnerField string
	// AccessTable defaults to group_access and may be schema-qualified.
	AccessTable string
	// AccessTargetField defaults to target_id.
	AccessTargetField string
	// AccessUserField defaults to user_id.
	AccessUserField string
	// AccessGroupField defaults to group_id.
	AccessGroupField string
	// AccessPermissionsField defaults to permissions.
	AccessPermissionsField string
	// AccessActivatedAtField defaults to activated_at and must be non-null
	// for an access row to contribute permissions.
	AccessActivatedAtField string
}

type plugin[ID any] struct {
	cfg Config
}

type listInput[ID any] struct {
	subject     Subject[ID]
	permissions []int32
	project     bool
}

// Item wraps a listed model with the effective permissions the subject holds
// on it. The model is carried in the Model field and the permissions in
// Permission.
type Item[T any] struct {
	Model      T       `json:"model"`
	Permission []int32 `json:"permission"`
}

// WithReBAC installs relationship-based access control on a sqlsee.Query.
func WithReBAC[ID any](cfg Config) sqlsee.Option {
	return sqlsee.WithPlugin(plugin[ID]{cfg: cfg})
}

// WithSubject supplies the subject and required permissions for one List
// call. Every permission must be granted for a row to be returned. The
// returned option filters rows without projecting permissions; use
// [List] to also surface the effective permissions per item.
func WithSubject[ID any](subject Subject[ID], permissions []int32) sqlsee.ListOption {
	subject.GroupIDs = append([]ID(nil), subject.GroupIDs...)
	return sqlsee.NewPluginOption(pluginName, listInput[ID]{
		subject:     subject,
		permissions: append([]int32(nil), permissions...),
		project:     false,
	})
}

// List returns a page whose items carry the effective permissions the subject
// has on each row, in addition to filtering by the required permissions. It is
// the opt-in entrypoint for the per-item permission projection; plain
// [sqlsee.Query.List] with [WithSubject] filters without projecting.
func List[T any, ID any](
	ctx context.Context,
	q *sqlsee.Query[T],
	req sqlsee.Request,
	subject Subject[ID],
	permissions []int32,
) (sqlsee.Page[Item[T]], error) {
	subject.GroupIDs = append([]ID(nil), subject.GroupIDs...)
	opt := sqlsee.NewPluginOption(pluginName, listInput[ID]{
		subject:     subject,
		permissions: append([]int32(nil), permissions...),
		project:     true,
	})

	page, err := q.ListWithExtras(ctx, req, opt)
	if err != nil {
		return sqlsee.Page[Item[T]]{}, err
	}

	items := make([]Item[T], len(page.Items))
	for i, model := range page.Items {
		var perms []int32
		if v, ok := page.Extras[i]["permission"].([]int32); ok {
			perms = v
		}
		items[i] = Item[T]{Model: model, Permission: perms}
	}

	return sqlsee.Page[Item[T]]{
		Items:      items,
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	}, nil
}

func (plugin[ID]) Name() string { return pluginName }

func (p plugin[ID]) Install(ctx sqlsee.PluginContext) (sqlsee.PluginHandler, error) {
	cfg := p.cfg
	applyDefaults(ctx.PrimaryKey(), &cfg)

	target, err := ctx.Column(cfg.TargetField)
	if err != nil {
		return nil, fmt.Errorf("target field: %w", err)
	}

	owner := ""
	if cfg.OwnerField != "" {
		owner, err = ctx.Column(cfg.OwnerField)
		if err != nil {
			return nil, fmt.Errorf("owner field: %w", err)
		}
	}

	joinSQL, err := buildPermissionJoin(target, cfg)
	if err != nil {
		return nil, err
	}
	whereSQL := buildWhere(owner)
	selectExpr := resultAlias + "." + resultColumn

	return func(_ context.Context, input sqlsee.PluginInput, builder *sqlsee.PluginBuilder) error {
		if !input.Present() {
			return fmt.Errorf("rebac.WithSubject is required")
		}

		value, ok := input.Value().(listInput[ID])
		if !ok {
			return fmt.Errorf("invalid rebac.WithSubject value")
		}
		if len(value.permissions) == 0 {
			return fmt.Errorf("permissions cannot be empty")
		}

		groups := value.subject.GroupIDs
		if groups == nil {
			groups = []ID{}
		}

		if err := builder.Join(joinSQL, value.subject.UserID, groups); err != nil {
			return err
		}

		if owner != "" {
			if err := builder.Where(whereSQL, value.subject.UserID, value.permissions); err != nil {
				return err
			}
		} else {
			if err := builder.Where(whereSQL, value.permissions); err != nil {
				return err
			}
		}

		if value.project {
			if err := builder.Select(selectExpr, projectionAlias); err != nil {
				return err
			}
		}

		return nil
	}, nil
}

const (
	accessAlias     = `"sqlsee_rebac_access"`
	unnestAlias     = `"sqlsee_rebac_perm"`
	unnestColumn    = `"value"`
	resultAlias     = `"sqlsee_rebac_permissions"`
	resultColumn    = `"permissions"`
	projectionAlias = "permission"
)

// buildPermissionJoin returns a LEFT JOIN LATERAL that computes, for each base
// row, the sorted deduplicated union of permissions granted to the subject via
// activated access rows. The subquery always returns exactly one row (array_agg
// over zero rows yields NULL, coalesced to an empty array), so the join never
// drops base rows. Placeholders are local to the clause: $1 is the subject's
// user id and $2 is the subject's group ids.
func buildPermissionJoin(target string, cfg Config) (string, error) {
	accessTable, err := sqlsee.QuoteQualifiedIdentifier(cfg.AccessTable)
	if err != nil {
		return "", fmt.Errorf("access table: %w", err)
	}

	accessTarget, err := accessColumn(accessAlias, cfg.AccessTargetField)
	if err != nil {
		return "", fmt.Errorf("access target field: %w", err)
	}
	accessUser, err := accessColumn(accessAlias, cfg.AccessUserField)
	if err != nil {
		return "", fmt.Errorf("access user field: %w", err)
	}
	accessGroup, err := accessColumn(accessAlias, cfg.AccessGroupField)
	if err != nil {
		return "", fmt.Errorf("access group field: %w", err)
	}
	accessPermissions, err := accessColumn(accessAlias, cfg.AccessPermissionsField)
	if err != nil {
		return "", fmt.Errorf("access permissions field: %w", err)
	}
	accessActivatedAt, err := accessColumn(accessAlias, cfg.AccessActivatedAtField)
	if err != nil {
		return "", fmt.Errorf("access activated-at field: %w", err)
	}

	perm := unnestAlias + "." + unnestColumn

	return fmt.Sprintf(`LEFT JOIN LATERAL (
  SELECT COALESCE(
    array_agg(DISTINCT %[1]s ORDER BY %[1]s),
    '{}'::int[]
  ) AS %[2]s
  FROM %[3]s AS %[4]s
  CROSS JOIN LATERAL unnest(%[5]s) AS %[6]s(%[7]s)
  WHERE %[8]s = %[9]s
    AND %[10]s IS NOT NULL
    AND (
      %[11]s = $1::uuid
      OR %[12]s = ANY($2::uuid[])
    )
) AS %[13]s ON true`,
		perm,
		resultColumn,
		accessTable,
		accessAlias,
		accessPermissions,
		unnestAlias,
		unnestColumn,
		accessTarget,
		target,
		accessActivatedAt,
		accessUser,
		accessGroup,
		resultAlias,
	), nil
}

// buildWhere returns the filter predicate referencing the lateral join's
// computed permissions. When owner is non-empty, ownership grants access
// without requiring an access row. Placeholders are local: without owner $1 is
// the required permissions; with owner $1 is the user id and $2 is the required
// permissions.
func buildWhere(owner string) string {
	permRef := resultAlias + "." + resultColumn
	if owner == "" {
		return permRef + " @> $1::int[]"
	}
	return owner + " = $1::uuid OR " + permRef + " @> $2::int[]"
}

func applyDefaults(primaryKey string, cfg *Config) {
	if cfg.TargetField == "" {
		cfg.TargetField = primaryKey
	}
	if cfg.AccessTable == "" {
		cfg.AccessTable = "group_access"
	}
	if cfg.AccessTargetField == "" {
		cfg.AccessTargetField = "target_id"
	}
	if cfg.AccessUserField == "" {
		cfg.AccessUserField = "user_id"
	}
	if cfg.AccessGroupField == "" {
		cfg.AccessGroupField = "group_id"
	}
	if cfg.AccessPermissionsField == "" {
		cfg.AccessPermissionsField = "permissions"
	}
	if cfg.AccessActivatedAtField == "" {
		cfg.AccessActivatedAtField = "activated_at"
	}
}

func accessColumn(alias, field string) (string, error) {
	quoted, err := sqlsee.QuoteIdentifier(field)
	if err != nil {
		return "", err
	}

	return alias + "." + quoted, nil
}
