package sqlsee

import (
	"fmt"
	"reflect"
	"sync"
)

type modelMeta struct {
	columns []string
	fields  map[string]fieldMeta
	err     error
}

var metadataCache sync.Map

func ColumnsFor[T any](alias string) ([]string, error) {
	typ := reflect.TypeFor[T]()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	meta := metadataFor(typ)
	if meta.err != nil {
		return nil, meta.err
	}

	columns := make([]string, len(meta.columns))
	for i, column := range meta.columns {
		columns[i] = quote(alias) + "." + quote(column) + " AS " + quote(column)
	}

	return columns, nil
}

func metadataFor(typ reflect.Type) modelMeta {
	if cached, ok := metadataCache.Load(typ); ok {
		return cached.(modelMeta)
	}

	meta := inspectModel(typ)
	actual, _ := metadataCache.LoadOrStore(typ, meta)

	return actual.(modelMeta)
}

func inspectModel(typ reflect.Type) modelMeta {
	if typ.Kind() != reflect.Struct {
		return modelMeta{
			err: fmt.Errorf("sqlsee: %s is not a struct", typ),
		}
	}

	meta := modelMeta{
		columns: make([]string, 0, typ.NumField()),
		fields:  map[string]fieldMeta{},
	}

	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		column := field.Tag.Get("db")
		if !field.IsExported() || column == "" || column == "-" {
			continue
		}

		if _, duplicate := meta.fields[column]; duplicate {
			return modelMeta{err: fmt.Errorf("sqlsee: duplicate db tag %q", column)}
		}

		meta.columns = append(meta.columns, column)
		meta.fields[column] = fieldMeta{index: i, typ: field.Type}
	}

	if len(meta.columns) == 0 {
		meta.err = fmt.Errorf("sqlsee: %s has no db tags; enable sqlc emit_db_tags", typ)
	}

	return meta
}

func (m modelMeta) value(model any, column string) (any, error) {
	field, ok := m.fields[column]
	if !ok {
		return nil, fmt.Errorf("sqlsee: unknown field %q", column)
	}

	return reflect.ValueOf(model).Field(field.index).Interface(), nil
}
