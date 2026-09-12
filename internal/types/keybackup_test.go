package types_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/beeper/babbleserv/internal/types"
)

func TestKeyBackupDataValidation(t *testing.T) {
	valid := `{"first_message_index":0,"forwarded_count":0,"is_verified":false,"session_data":{"ciphertext":"encrypted"}}`
	var data types.KeyBackupData
	require.NoError(t, json.Unmarshal([]byte(valid), &data))
	require.JSONEq(t, `{"ciphertext":"encrypted"}`, string(data.SessionData))
	for _, invalid := range []string{
		`{}`, `null`,
		`{"forwarded_count":0,"is_verified":false,"session_data":{}}`,
		`{"first_message_index":0,"is_verified":false,"session_data":{}}`,
		`{"first_message_index":0,"forwarded_count":0,"session_data":{}}`,
		`{"first_message_index":0,"forwarded_count":0,"is_verified":false}`,
		`{"first_message_index":0,"forwarded_count":0,"is_verified":null,"session_data":{}}`,
		`{"first_message_index":-1,"forwarded_count":0,"is_verified":false,"session_data":{}}`,
		`{"first_message_index":0,"forwarded_count":-1,"is_verified":false,"session_data":{}}`,
		`{"first_message_index":0,"forwarded_count":0,"is_verified":false,"session_data":null}`,
		`{"first_message_index":0,"forwarded_count":0,"is_verified":false,"session_data":[]}`,
	} {
		t.Run(invalid, func(t *testing.T) { require.Error(t, json.Unmarshal([]byte(invalid), &data)) })
	}
	require.NoError(t, json.Unmarshal([]byte(valid), &data))
	restored, err := types.NewKeyBackupDataFromBytes(data.ToBytes())
	require.NoError(t, err)
	require.Equal(t, &data, restored)
}
