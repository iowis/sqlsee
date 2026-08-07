package sqlsee

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

func queryFingerprint(
	table string,
	sorts []Sort,
	filters []FilterGroup,
	scope any,
) (string, error) {
	raw, err := json.Marshal(struct {
		Table   string        `json:"table"`
		Sorts   []Sort        `json:"sorts"`
		Filters []FilterGroup `json:"filters"`
		Scope   any           `json:"scope"`
	}{table, sorts, filters, scope})
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:]), nil
}

type fieldMeta struct {
	index int
	typ   reflect.Type
}

func decodeValue(raw json.RawMessage, typ reflect.Type) (any, error) {
	value := reflect.New(typ)
	if err := json.Unmarshal(raw, value.Interface()); err != nil {
		return nil, err
	}

	return value.Elem().Interface(), nil
}

func isNull(value any) bool {
	if value == nil {
		return true
	}

	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return true
		}

		rv = rv.Elem()
	}

	if rv.Kind() == reflect.Struct {
		valid := rv.FieldByName("Valid")

		return valid.IsValid() && valid.Kind() == reflect.Bool && !valid.Bool()
	}

	return false
}

func allowedSet(
	fields []string,
	meta modelMeta,
) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if _, ok := meta.fields[field]; !ok {
			return nil, fmt.Errorf("sqlsee: unknown configured field %q", field)
		}

		set[field] = struct{}{}
	}

	return set, nil
}

func quote(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// QuoteIdentifier safely quotes a single PostgreSQL identifier.
func QuoteIdentifier(identifier string) (string, error) {
	if identifier == "" || strings.ContainsRune(identifier, '\x00') {
		return "", fmt.Errorf("sqlsee: invalid identifier %q", identifier)
	}

	return quote(identifier), nil
}

// QuoteQualifiedIdentifier safely quotes a dot-qualified PostgreSQL identifier.
func QuoteQualifiedIdentifier(identifier string) (string, error) {
	parts := strings.Split(identifier, ".")
	for i, part := range parts {
		quoted, err := QuoteIdentifier(part)
		if err != nil {
			return "", fmt.Errorf("sqlsee: invalid identifier %q", identifier)
		}
		parts[i] = quoted
	}

	return strings.Join(parts, "."), nil
}

func quoteQualified(identifier string) (string, error) {
	return QuoteQualifiedIdentifier(identifier)
}
