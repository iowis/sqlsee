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
}

// WithReBAC installs relationship-based access control on a sqlsee Query.
func WithReBAC[ID any](cfg Config) sqlsee.Option {
	return sqlsee.WithPlugin(plugin[ID]{cfg: cfg})
}

// WithSubject supplies the subject and required permissions for one List call.
// Every permission must be granted for a row to be returned.
func WithSubject[ID any](subject Subject[ID], permissions []int32) sqlsee.ListOption {
	subject.GroupIDs = append([]ID(nil), subject.GroupIDs...)
	return sqlsee.NewPluginOption(pluginName, listInput[ID]{
		subject:     subject,
		permissions: append([]int32(nil), permissions...),
	})
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

	predicate, err := buildPredicate(target, owner, cfg)
	if err != nil {
		return nil, err
	}

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

		return builder.Where(
			predicate,
			value.subject.UserID,
			groups,
			value.permissions,
		)
	}, nil
}

func buildPredicate(target, owner string, cfg Config) (string, error) {
	accessTable, err := sqlsee.QuoteQualifiedIdentifier(cfg.AccessTable)
	if err != nil {
		return "", fmt.Errorf("access table: %w", err)
	}

	const (
		accessAlias     = `"sqlsee_rebac_access"`
		permissionAlias = `"sqlsee_rebac_permission"`
		permissionValue = `"value"`
	)
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

	permission := permissionAlias + "." + permissionValue
	predicate := fmt.Sprintf(`(
SELECT COALESCE(
  array_agg(DISTINCT %[1]s ORDER BY %[1]s),
  '{}'::int[]
)
FROM %[2]s AS %[3]s
CROSS JOIN LATERAL unnest(%[4]s) AS %[5]s(%[6]s)
WHERE %[7]s = %[8]s
  AND %[9]s IS NOT NULL
  AND (
    %[10]s = $1::uuid
    OR %[11]s = ANY($2::uuid[])
  )
) @> $3::int[]`,
		permission,
		accessTable,
		accessAlias,
		accessPermissions,
		permissionAlias,
		permissionValue,
		accessTarget,
		target,
		accessActivatedAt,
		accessUser,
		accessGroup,
	)
	if owner != "" {
		predicate = owner + " = $1::uuid OR " + predicate
	}

	return predicate, nil
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
