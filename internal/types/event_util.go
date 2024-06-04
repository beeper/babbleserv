package types

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/id"
)

func calculateEventID(eventJSON []byte, roomVersion gomatrixserverlib.RoomVersion) (id.EventID, error) {
	verImpl, err := gomatrixserverlib.GetRoomVersion(roomVersion)
	if err != nil {
		return "", err
	}
	redactedJSON, err := verImpl.RedactEventJSON(eventJSON)
	if err != nil {
		return "", err
	}

	var event map[string]spec.RawJSON
	if err = json.Unmarshal(redactedJSON, &event); err != nil {
		return "", err
	}

	delete(event, "signatures")
	delete(event, "unsigned")

	hashableEventJSON, err := json.Marshal(event)
	if err != nil {
		return "", err
	}

	hashableEventJSON, err = gomatrixserverlib.CanonicalJSON(hashableEventJSON)
	if err != nil {
		return "", err
	}

	sha256Hash := sha256.Sum256(hashableEventJSON)
	var eventID string

	eventFormat := verImpl.EventFormat()
	eventIDFormat := verImpl.EventIDFormat()

	switch eventFormat {
	case gomatrixserverlib.EventFormatV1:
		if err = json.Unmarshal(event["event_id"], &eventID); err != nil {
			return "", err
		}
	case gomatrixserverlib.EventFormatV2:
		var encoder *base64.Encoding
		switch eventIDFormat {
		case gomatrixserverlib.EventIDFormatV2:
			encoder = base64.RawStdEncoding.WithPadding(base64.NoPadding)
		case gomatrixserverlib.EventIDFormatV3:
			encoder = base64.RawURLEncoding.WithPadding(base64.NoPadding)
		default:
			return "", gomatrixserverlib.UnsupportedRoomVersionError{Version: roomVersion}
		}
		eventID = fmt.Sprintf("$%s", encoder.EncodeToString(sha256Hash[:]))
	default:
		return "", gomatrixserverlib.UnsupportedRoomVersionError{Version: roomVersion}
	}

	return id.EventID(eventID), nil
}

// SignEvent adds a ED25519 signature to the event for the given key.
func signEvent(signingName string, keyID gomatrixserverlib.KeyID, privateKey ed25519.PrivateKey, eventJSON []byte, roomVersion gomatrixserverlib.RoomVersion) ([]byte, error) {
	verImpl, err := gomatrixserverlib.GetRoomVersion(roomVersion)
	if err != nil {
		return nil, err
	}
	// Redact the event before signing so signature that will remain valid even if the event is redacted.
	redactedJSON, err := verImpl.RedactEventJSON(eventJSON)
	if err != nil {
		return nil, err
	}

	// Sign the JSON, this adds a "signatures" key to the redacted event.
	// TODO: Make an internal version of SignJSON that returns just the signatures so that we don't have to parse it out of the JSON.
	signedJSON, err := gomatrixserverlib.SignJSON(signingName, keyID, privateKey, redactedJSON)
	if err != nil {
		return nil, err
	}

	// Extract the signatures and inject into the full (non-redacted) JSON
	signatures := gjson.GetBytes(signedJSON, "signatures").String()
	sjson.SetRawBytes(eventJSON, "signatures", []byte(signatures))

	return eventJSON, nil
	// var signedEvent struct {
	// 	Signatures spec.RawJSON `json:"signatures"`
	// }
	// if err := json.Unmarshal(signedJSON, &signedEvent); err != nil {
	// 	return nil, err
	// }

	// // Unmarshal the event JSON so that we can replace the signatures key.
	// var event map[string]spec.RawJSON
	// if err := json.Unmarshal(eventJSON, &event); err != nil {
	// 	return nil, err
	// }

	// event["signatures"] = signedEvent.Signatures

	// return json.Marshal(event)
}
