package sqlsee

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type cursorPayload struct {
	Version int               `json:"v"`
	Query   string            `json:"q"`
	Keys    []json.RawMessage `json:"k"`
}

func encodeCursor(query string, values []any, secret []byte) (string, error) {
	payload := cursorPayload{
		Version: 1,
		Query:   query,
		Keys:    make([]json.RawMessage, len(values)),
	}

	for i, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("sqlsee: encode cursor: %w", err)
		}

		payload.Keys[i] = raw
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	if len(secret) > 0 {
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(raw)
		raw = append(mac.Sum(nil), raw...)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(value string, secret []byte) (cursorPayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursorPayload{}, fmt.Errorf("sqlsee: invalid cursor")
	}

	if len(secret) > 0 {
		if len(raw) <= sha256.Size {
			return cursorPayload{}, fmt.Errorf("sqlsee: invalid cursor")
		}

		signature, body := raw[:sha256.Size], raw[sha256.Size:]
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(body)

		if !hmac.Equal(signature, mac.Sum(nil)) {
			return cursorPayload{}, fmt.Errorf("sqlsee: invalid cursor signature")
		}

		raw = body
	}

	var payload cursorPayload
	if json.Unmarshal(raw, &payload) != nil || payload.Version != 1 {
		return cursorPayload{}, fmt.Errorf("sqlsee: invalid cursor")
	}

	return payload, nil
}
