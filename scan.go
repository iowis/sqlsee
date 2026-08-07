package sqlsee

import (
	"reflect"

	"github.com/jackc/pgx/v5"
)

// collectRows scans rows into model values of type T. When collectExtras is
// true, columns not belonging to the model are captured into per-row maps
// keyed by the column's field-description name (the plugin's declared alias).
// When false, extra columns are ignored and the well-tested
// pgx.RowToStructByName mapper is used.
func collectRows[T any](
	rows pgx.Rows,
	meta modelMeta,
	collectExtras bool,
) ([]T, []map[string]any, error) {
	if !collectExtras {
		items, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
		return items, nil, err
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	type colPlan struct {
		isModel  bool
		fieldIdx int
		alias    string
	}
	plans := make([]colPlan, len(fields))
	for i, f := range fields {
		if fm, ok := meta.fields[f.Name]; ok {
			plans[i] = colPlan{isModel: true, fieldIdx: fm.index}
		} else {
			plans[i] = colPlan{alias: f.Name}
		}
	}

	var items []T
	var extras []map[string]any
	for rows.Next() {
		model := reflect.New(meta.typ).Elem()
		dest := make([]any, len(fields))
		rowExtras := make(map[string]any, 0)
		for i, p := range plans {
			if p.isModel {
				dest[i] = model.Field(p.fieldIdx).Addr().Interface()
			} else {
				var v any
				dest[i] = &v
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		for i, p := range plans {
			if !p.isModel {
				rowExtras[p.alias] = *(dest[i].(*any))
			}
		}
		items = append(items, model.Interface().(T))
		extras = append(extras, rowExtras)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return items, extras, nil
}
