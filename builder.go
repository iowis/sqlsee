package sqlsee

import "strconv"

type sqlBuilder struct {
	args  []any
	where []string
}

func (b *sqlBuilder) arg(value any) string {
	b.args = append(b.args, value)

	return "$" + strconv.Itoa(len(b.args))
}
