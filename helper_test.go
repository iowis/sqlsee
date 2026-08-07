package sqlsee

import (
	"testing"

	"github.com/stretchr/testify/require"
)
func TestQuoteIdentifier(t *testing.T) {
	quoted, err := QuoteIdentifier(`value"quoted`)
	require.NoError(t, err)
	require.Equal(t, `"value""quoted"`, quoted)

	quoted, err = QuoteIdentifier("")
	require.Empty(t, quoted)
	require.EqualError(t, err, `sqlsee: invalid identifier ""`)

	quoted, err = QuoteIdentifier("bad\x00identifier")
	require.Empty(t, quoted)
	require.EqualError(t, err, "sqlsee: invalid identifier \"bad\\x00identifier\"")
}

func TestQuoteQualifiedIdentifier(t *testing.T) {
	quoted, err := QuoteQualifiedIdentifier(`auth.group"access`)
	require.NoError(t, err)
	require.Equal(t, `"auth"."group""access"`, quoted)

	quoted, err = QuoteQualifiedIdentifier("auth..access")
	require.Empty(t, quoted)
	require.EqualError(t, err, `sqlsee: invalid identifier "auth..access"`)
}
