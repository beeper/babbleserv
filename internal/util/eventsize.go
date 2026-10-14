package util

import (
	"encoding/json"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/matrix-org/gomatrixserverlib"

	"github.com/beeper/babbleserv/internal/types"
)

const (
	MaxPDUBytes     = 65536
	MaxPDUFieldSize = 255
)

// CheckEventSize fails with a 413 EventValidationError for an event over Synapse's PDU limits: its
// canonical JSON over MaxPDUBytes, or its sender, room ID, event ID, type or state key over
// MaxPDUFieldSize, counted in codepoints before room version 11 and in bytes from it.
func CheckEventSize(ev *types.Event) error {
	b, err := json.Marshal(ev)
	if err == nil {
		b, err = gomatrixserverlib.CanonicalJSON(b)
	}
	if err != nil {
		return err
	} else if len(b) > MaxPDUBytes {
		return eventTooLarge("event is %d bytes, over %d", len(b), MaxPDUBytes)
	}

	fieldSize := utf8.RuneCountInString
	if roomVersion, err := gomatrixserverlib.GetRoomVersion(ev.GetRoomVersion()); err == nil && roomVersion.StrictEventByteLimits() {
		fieldSize = func(s string) int { return len(s) }
	}
	fields := map[string]string{
		"sender":   ev.Sender.String(),
		"room_id":  ev.RoomID.String(),
		"event_id": ev.ID.String(),
		"type":     ev.Type.Type,
	}
	if ev.StateKey != nil {
		fields["state_key"] = *ev.StateKey
	}
	for name, value := range fields {
		if size := fieldSize(value); size > MaxPDUFieldSize {
			return eventTooLarge("%s is %d long, over %d", name, size, MaxPDUFieldSize)
		}
	}
	return nil
}

func eventTooLarge(format string, args ...any) error {
	return &gomatrixserverlib.EventValidationError{Code: http.StatusRequestEntityTooLarge, Message: fmt.Sprintf(format, args...)}
}
