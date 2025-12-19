package util

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func EventForClientAPI(ev *types.Event) *types.Event {
	ev.IsForClientAPI = true
	return ev
}

func EventsForClientAPI(evs []*types.Event) []*types.Event {
	for _, ev := range evs {
		ev.IsForClientAPI = true
	}
	return evs
}

func EventsToPartialEvents(evs []*types.Event) []*types.PartialEvent {
	partEvs := make([]*types.PartialEvent, len(evs))
	for i, ev := range evs {
		partEvs[i] = &ev.PartialEvent
	}
	return partEvs
}

func EventsToMauPDUs(evs []*types.Event) []federation.PDU {
	pdus := make([]federation.PDU, len(evs))
	for i, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			panic(err)
		}
		pdus[i] = b
	}
	return pdus
}

func SortEventList(evs []*types.Event) {
	slices.SortFunc(evs, func(a, b *types.Event) int {
		// TODO: this is probably not enough
		if a.Depth > b.Depth {
			return 1
		}
		if a.Depth == b.Depth && len(a.AuthEventIDs) > len(b.AuthEventIDs) {
			return 1
		}
		return -1
	})
}

func EventsToPDUs(evs []*types.Event) []gomatrixserverlib.PDU {
	pdus := make([]gomatrixserverlib.PDU, len(evs))
	for i, ev := range evs {
		pdus[i] = ev.PDU()
	}
	return pdus
}

func EventsToJSONs(evs []*types.Event) []json.RawMessage {
	jsons := make([]json.RawMessage, len(evs))
	for i, ev := range evs {
		json, err := json.Marshal(ev)
		if err != nil {
			panic(err)
		}
		jsons[i] = json
	}
	return jsons
}

func MergeEventsMap(evSlices ...[]*types.Event) map[id.EventID]*types.Event {
	evMap := make(map[id.EventID]*types.Event, len(evSlices)*len(evSlices[0]))
	for _, evs := range evSlices {
		for _, ev := range evs {
			evMap[ev.ID] = ev
		}
	}
	return evMap
}

func getEventRedactedJSON(ev *types.Event) ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}

	if b, err = ev.MustGetRoomSpec().RedactEventJSON(b); err != nil {
		return nil, err
	}

	return b, nil
}

func GetEventSignature(ev *types.Event, key ed25519.PrivateKey) (string, error) {
	b, err := getEventRedactedJSON(ev)
	if err != nil {
		return "", err
	}
	return GetJSONSignature(b, key)
}

func GetEventReferenceHash(ev *types.Event) (id.EventID, error) {
	b, err := getEventRedactedJSON(ev)
	if err != nil {
		return "", err
	}
	return GetRefHashForRedactedBytes(b, ev.GetRoomVersion())
}

func GetEventSignatureAndRefrerenceHash(ev *types.Event, key ed25519.PrivateKey) (string, id.EventID, error) {
	b, err := getEventRedactedJSON(ev)
	if err != nil {
		return "", "", err
	}
	signature, err := GetJSONSignature(b, key)
	if err != nil {
		return "", "", err
	}
	refHash, err := GetRefHashForRedactedBytes(b, ev.GetRoomVersion())
	if err != nil {
		return "", "", err
	}
	return signature, refHash, nil
}

func GetEventContentHash(ev *types.Event) (string, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	return GetJSONContentHash(b)
}

// Validates an event a good content hash and signature, will also generate the
// ID (reference hash) and either set or check it depending on whether the input
// event has any populated.
func VerifyEvent(
	ctx context.Context,
	ev *types.Event,
	sendingServerName string,
	keyStore *KeyStore,
) (error, error) {
	b, err := getEventRedactedJSON(ev)
	if err != nil {
		return nil, err
	}

	refHash, err := GetRefHashForRedactedBytes(b, gomatrixserverlib.RoomVersion(ev.RoomVersion))
	if err != nil {
		return nil, err
	} else if ev.ID == "" {
		ev.ID = refHash
	} else if refHash != ev.ID {
		return errors.New("event ID is not reference hash"), err
	}

	verifyErr := keyStore.VerifyJSONFromServer(ctx, sendingServerName, b)
	if verifyErr != nil {
		return verifyErr, nil
	}

	// Finally check the content hash matches, if not this means the event has been redacted
	contentHash, err := GetEventContentHash(ev)
	if err != nil {
		return nil, err
	} else if hash, found := ev.Hashes["sha256"]; !found || hash != contentHash {
		ev.Redacted = true
		return types.ErrEventRedacted, nil
	}

	return nil, nil
}

// https://spec.matrix.org/v1.10/server-server-api/#calculating-the-reference-hash-for-an-event
func GetRefHashForRedactedBytes(b []byte, roomVersion gomatrixserverlib.RoomVersion) (id.EventID, error) {
	roomSpec, err := gomatrixserverlib.GetRoomVersion(roomVersion)
	if err != nil {
		return "", err
	}

	if b, err = sjson.DeleteBytes(b, "signatures"); err != nil {
		return "", err
	}
	if b, err = sjson.DeleteBytes(b, "unsigned"); err != nil {
		return "", err
	}

	if b, err = gomatrixserverlib.CanonicalJSON(b); err != nil {
		return "", err
	}

	sha256Hash := sha256.Sum256(b)

	var eventID string
	eventFormat := roomSpec.EventFormat()
	eventIDFormat := roomSpec.EventIDFormat()

	switch eventFormat {
	case gomatrixserverlib.EventFormatV1:
		return "", gomatrixserverlib.UnsupportedRoomVersionError{Version: roomVersion}
	case gomatrixserverlib.EventFormatV2:
		switch eventIDFormat {
		case gomatrixserverlib.EventIDFormatV2:
			eventID = "$" + Base64Encode(sha256Hash[:])
		case gomatrixserverlib.EventIDFormatV3:
			eventID = "$" + Base64EncodeURLSafe(sha256Hash[:])
		default:
			return "", gomatrixserverlib.UnsupportedRoomVersionError{Version: roomVersion}
		}
	default:
		return "", gomatrixserverlib.UnsupportedRoomVersionError{Version: roomVersion}
	}

	return id.EventID(eventID), nil
}

func HashAndSignEvent(ev *types.Event, serverName, keyID string, key ed25519.PrivateKey) error {
	// Calculate the content hash before the ID/reference hash
	hash, err := GetEventContentHash(ev)
	if err != nil {
		return err
	}
	ev.Hashes = map[string]string{"sha256": hash}

	// Calculate the signature & ID/reference hash
	signature, refHash, err := GetEventSignatureAndRefrerenceHash(ev, key)
	if err != nil {
		return err
	}
	ev.Signatures = map[string]map[string]string{
		serverName: {
			keyID: signature,
		},
	}
	ev.ClientEvent.ID = refHash

	return nil
}
