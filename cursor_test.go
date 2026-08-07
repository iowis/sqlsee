package sqlsee

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCursorRoundTrip(t *testing.T) {
	secret := []byte("test cursor secret")
	encoded, err := encodeCursor("query-key", []any{"Alpha", 42}, secret)
	require.NoError(t, err)

	payload, err := decodeCursor(encoded, secret)
	require.NoError(t, err)
	require.Equal(t, "query-key", payload.Query)
	require.JSONEq(t, `"Alpha"`, string(payload.Keys[0]))
	require.JSONEq(t, `42`, string(payload.Keys[1]))
}

func TestCursorRejectsTampering(t *testing.T) {
	secret := []byte("test cursor secret")
	encoded, err := encodeCursor("query-key", []any{42}, secret)
	require.NoError(t, err)

	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(t, err)
	raw[len(raw)-1] ^= 1
	tampered := base64.RawURLEncoding.EncodeToString(raw)

	_, err = decodeCursor(tampered, secret)
	require.EqualError(t, err, "sqlsee: invalid cursor signature")
}

func TestCursorRejectsInvalidInput(t *testing.T) {
	_, err := decodeCursor("not a cursor!", nil)
	require.EqualError(t, err, "sqlsee: invalid cursor")

	encoded := base64.RawURLEncoding.EncodeToString([]byte(`{"v":2,"q":"query","k":[]}`))
	_, err = decodeCursor(encoded, nil)
	require.EqualError(t, err, "sqlsee: invalid cursor")
}
