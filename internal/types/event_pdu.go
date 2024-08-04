package types

import (
	"crypto/ed25519"
	"encoding/json"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/log"
)

// Wrapper around an Event that implements the PDU interface from gomatrixserverlib:
// https://pkg.go.dev/github.com/matrix-org/gomatrixserverlib#PDU

type EventPDU struct {
	ev          *Event
	roomVersion gomatrixserverlib.RoomVersion
}

func (pdu EventPDU) MarshalJSON() ([]byte, error) {
	// return json.Marshal(pdu.ev)
	b, err := json.Marshal(pdu.ev)
	if err != nil {
		return nil, err
	}
	log.Info().Str("DATA", string(b)).Msg("SIGNED")
	return b, err
	// return gomatrixserverlib.CanonicalJSON(b)
}

func (pdu EventPDU) Event() *Event {
	return pdu.ev
}

func (pdu EventPDU) EventID() string {
	return string(pdu.ev.ID)
}

func (pdu EventPDU) StateKey() *string {
	return pdu.ev.StateKey
}

func (pdu EventPDU) Type() string {
	return pdu.ev.Type.Type
}

func (pdu EventPDU) Content() []byte {
	return pdu.ev.Content
}

func (pdu EventPDU) StateKeyEquals(s string) bool {
	if pdu.ev.StateKey == nil {
		return false
	}
	return s == *pdu.ev.StateKey
}

// // JoinRule returns the value of the content.join_rule field if this event
// // is an "m.room.join_rules" event.
// // Returns an error if the event is not a m.room.join_rules event or if the content
// // is not valid m.room.join_rules content.
func (pdu EventPDU) JoinRule() (string, error) {
	return "", nil
}

// // HistoryVisibility returns the value of the content.history_visibility field if this event
// // is an "m.room.history_visibility" event.
// // Returns an error if the event is not a m.room.history_visibility event or if the content
// // is not valid m.room.history_visibility content.
func (pdu EventPDU) HistoryVisibility() (gomatrixserverlib.HistoryVisibility, error) {
	return "", nil
}

func (pdu EventPDU) Membership() (string, error) {
	return string(pdu.ev.Membership()), nil
}

func (pdu EventPDU) PowerLevels() (*gomatrixserverlib.PowerLevelContent, error) {
	return nil, nil
}

func (pdu EventPDU) Version() gomatrixserverlib.RoomVersion {
	return pdu.roomVersion
}

func (pdu EventPDU) RoomID() spec.RoomID {
	roomID, _ := spec.NewRoomID(string(pdu.ev.RoomID))
	return *roomID
}

func (pdu EventPDU) Redacts() string {
	return ""
}

// // Redacted returns whether the event is redacted.

func (pdu EventPDU) Redacted() bool {
	return false
}

func (pdu EventPDU) PrevEventIDs() []string {
	eventIDs := make([]string, 0, len(pdu.ev.PrevEventIDs))
	for _, eventID := range pdu.ev.PrevEventIDs {
		eventIDs = append(eventIDs, eventID.String())
	}
	return eventIDs
}

func (pdu EventPDU) OriginServerTS() spec.Timestamp {
	return spec.Timestamp(pdu.ev.Timestamp)
}

// // Redact redacts the event.

func (pdu EventPDU) Redact() {}

func (pdu EventPDU) SenderID() spec.SenderID {
	return spec.SenderID(pdu.ev.Sender)
}

func (pdu EventPDU) Unsigned() []byte {
	return nil
}

// // SetUnsigned sets the unsigned key of the event.
// // Returns a copy of the event with the "unsigned" key set.

func (pdu EventPDU) SetUnsigned(unsigned any) (gomatrixserverlib.PDU, error) {
	return pdu, nil
}

// // SetUnsignedField takes a path and value to insert into the unsigned dict of
// // the event.
// // path is a dot separated path into the unsigned dict (see gjson package
// // for details on format). In particular some characters like '.' and '*' must
// // be escaped.
func (pdu EventPDU) SetUnsignedField(path string, value any) error {
	return nil
}

// // Sign returns a copy of the event with an additional signature.
func (pdu EventPDU) Sign(signingName string, keyID gomatrixserverlib.KeyID, privateKey ed25519.PrivateKey) gomatrixserverlib.PDU {
	return nil
}

func (pdu EventPDU) Depth() int64 {
	return pdu.ev.Depth
}

func (pdu EventPDU) JSON() []byte {
	b, err := json.Marshal(pdu.ev)
	if err != nil {
		panic("failed to JSON marshal event")
	}
	return b
}

func (pdu EventPDU) AuthEventIDs() []string {
	eventIDs := make([]string, 0, len(pdu.ev.AuthEventIDs))
	for _, eventID := range pdu.ev.AuthEventIDs {
		eventIDs = append(eventIDs, eventID.String())
	}
	return eventIDs
}

func (pdu EventPDU) ToHeaderedJSON() ([]byte, error) {
	return nil, nil
}
