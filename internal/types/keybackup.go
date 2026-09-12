package types

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/vmihailenco/msgpack/v5"
)

// KeyBackupAlgorithm represents the backup algorithm used
type KeyBackupAlgorithm string

const (
	KeyBackupAlgorithmMegolmV1Curve25519 KeyBackupAlgorithm = "m.megolm_backup.v1.curve25519-aes-sha2"
)

// KeyBackupVersion represents the metadata for a key backup version
type KeyBackupVersion struct {
	Algorithm KeyBackupAlgorithm `json:"algorithm"`
	AuthData  json.RawMessage    `json:"auth_data"`
}

func NewKeyBackupVersionFromBytes(b []byte) (*KeyBackupVersion, error) {
	var v KeyBackupVersion
	if err := msgpack.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func MustNewKeyBackupVersionFromBytes(b []byte) *KeyBackupVersion {
	v, err := NewKeyBackupVersionFromBytes(b)
	if err != nil {
		panic(err)
	}
	return v
}

func (v *KeyBackupVersion) ToBytes() []byte {
	b, err := msgpack.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// KeyBackupVersionWithMeta includes the version string and metadata
type KeyBackupVersionWithMeta struct {
	Version   string             `json:"version"`
	Algorithm KeyBackupAlgorithm `json:"algorithm"`
	AuthData  json.RawMessage    `json:"auth_data"`
	Count     int                `json:"count"`
	ETag      string             `json:"etag"`
}

// KeyBackupData represents the encrypted session data for a single key
type KeyBackupData struct {
	FirstMessageIndex int             `json:"first_message_index" msgpack:"fmi"`
	ForwardedCount    int             `json:"forwarded_count" msgpack:"fc"`
	IsVerified        bool            `json:"is_verified" msgpack:"iv"`
	SessionData       json.RawMessage `json:"session_data" msgpack:"sd"`
}

func NewKeyBackupDataFromBytes(b []byte) (*KeyBackupData, error) {
	var d KeyBackupData
	if err := msgpack.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func MustNewKeyBackupDataFromBytes(b []byte) *KeyBackupData {
	d, err := NewKeyBackupDataFromBytes(b)
	if err != nil {
		panic(err)
	}
	return d
}

func (d *KeyBackupData) ToBytes() []byte {
	b, err := msgpack.Marshal(d)
	if err != nil {
		panic(err)
	}
	return b
}

// KeyBackupUpdateResponse is returned when storing keys
type KeyBackupUpdateResponse struct {
	ETag  string `json:"etag"`
	Count int    `json:"count"`
}

// Request/response types for API endpoints

// ReqCreateKeyBackupVersion is the request body for POST /room_keys/version
type ReqCreateKeyBackupVersion struct {
	Algorithm KeyBackupAlgorithm `json:"algorithm"`
	AuthData  json.RawMessage    `json:"auth_data"`
}

// RespCreateKeyBackupVersion is the response for POST /room_keys/version
type RespCreateKeyBackupVersion struct {
	Version string `json:"version"`
}

// ReqUpdateKeyBackupVersion is the request body for PUT /room_keys/version/{version}
type ReqUpdateKeyBackupVersion struct {
	Algorithm KeyBackupAlgorithm `json:"algorithm"`
	AuthData  json.RawMessage    `json:"auth_data"`
}

// RoomKeyBackup contains backup data for a single room
type RoomKeyBackup struct {
	Sessions map[string]*KeyBackupData `json:"sessions"`
}

// ReqStoreRoomKeys is the request body for PUT /room_keys/keys
type ReqStoreRoomKeys struct {
	Rooms map[string]*RoomKeyBackup `json:"rooms"`
}

// RespGetRoomKeys is the response for GET /room_keys/keys
type RespGetRoomKeys struct {
	Rooms map[string]*RoomKeyBackup `json:"rooms"`
}

// RespGetRoomKeysByRoom is the response for GET /room_keys/keys/{roomId}
type RespGetRoomKeysByRoom struct {
	Sessions map[string]*KeyBackupData `json:"sessions"`
}

var (
	ErrKeyBackupNotFound          = errors.New("unknown backup version")
	ErrKeyBackupAlgorithmMismatch = errors.New("algorithm does not match backup")
)

type WrongKeyBackupVersionError struct{ CurrentVersion string }

func (e *WrongKeyBackupVersionError) Error() string { return "wrong backup version" }

// UnmarshalJSON preserves the distinction between absent fields and valid zero values.
func (d *KeyBackupData) UnmarshalJSON(data []byte) error {
	var wire struct {
		FirstMessageIndex *int            `json:"first_message_index"`
		ForwardedCount    *int            `json:"forwarded_count"`
		IsVerified        *bool           `json:"is_verified"`
		SessionData       json.RawMessage `json:"session_data"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.FirstMessageIndex == nil || wire.ForwardedCount == nil || wire.IsVerified == nil ||
		*wire.FirstMessageIndex < 0 || *wire.ForwardedCount < 0 || !isJSONObject(wire.SessionData) {
		return errors.New("invalid key backup data")
	}
	*d = KeyBackupData{FirstMessageIndex: *wire.FirstMessageIndex, ForwardedCount: *wire.ForwardedCount,
		IsVerified: *wire.IsVerified, SessionData: wire.SessionData}
	return nil
}

func isJSONObject(data []byte) bool {
	data = bytes.TrimSpace(data)
	return len(data) > 0 && data[0] == '{' && json.Valid(data)
}

func (r *RoomKeyBackup) Valid() bool {
	if r == nil || r.Sessions == nil {
		return false
	}
	for _, session := range r.Sessions {
		if session == nil {
			return false
		}
	}
	return true
}
