package util

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

func EventsToIDs(evs []*types.Event) []id.EventID {
	ids := make([]id.EventID, len(evs))
	for i, ev := range evs {
		ids[i] = ev.ID
	}
	return ids
}

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
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(len(a.AuthEventIDs), len(b.AuthEventIDs)))
	})
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
	keyStore *KeyStore,
) (error, error) {
	return verifyEvent(ctx, ev, "", keyStore)
}

// An invite request is checked before the invited server adds its signature.
func VerifyEventFromServer(ctx context.Context, ev *types.Event, serverName string, keyStore *KeyStore) (error, error) {
	return verifyEvent(ctx, ev, serverName, keyStore)
}

// Verifies an event from another server, returning the event to keep: the redacted form of one that
// has been redacted, as Synapse does.
func VerifyRemoteEvent(ctx context.Context, ev *types.Event, keyStore *KeyStore) (*types.Event, error, error) {
	// The event must be signed by the senders server, which may not be the one it came from
	verifyErr, err := VerifyEvent(ctx, ev, keyStore)
	if err != nil {
		return nil, nil, err
	} else if errors.Is(verifyErr, types.ErrEventRedacted) {
		redactedEv, err := ev.GetRedactedEvent()
		if err != nil {
			return nil, nil, err
		}
		return redactedEv, nil, nil
	} else if verifyErr != nil {
		return nil, verifyErr, nil
	}
	return ev, nil, nil
}

// Parses and verifies events fetched from another server, skipping those that cannot be parsed or
// fail verification and any already seen. Redacted events are kept, redacted.
func VerifyRemoteEvents[S ~[]E, E ~[]byte](
	ctx context.Context,
	raws S,
	roomVersion string,
	keyStore *KeyStore,
	seenIDs map[id.EventID]struct{},
) ([]*types.Event, error) {
	log := zerolog.Ctx(ctx)
	evs := make([]*types.Event, 0, len(raws))
	for _, b := range raws {
		remoteEv := &types.Event{RoomVersion: roomVersion}
		if err := json.Unmarshal(b, remoteEv); err != nil {
			log.Warn().Err(err).Msg("Skipping remote event that cannot be parsed")
			continue
		}
		verifiedEv, verifyErr, err := VerifyRemoteEvent(ctx, remoteEv, keyStore)
		if err != nil {
			return nil, err
		} else if verifyErr != nil {
			log.Err(verifyErr).
				Stringer("event_id", remoteEv.ID).
				Stringer("type", remoteEv.Type).
				Any("ev", remoteEv).
				Msg("Skipping remote event that failed verification")
			continue
		} else if _, found := seenIDs[verifiedEv.ID]; found {
			log.Warn().
				Stringer("event_id", verifiedEv.ID).
				Msg("Skipping duplicate remote event")
			continue
		}
		evs = append(evs, verifiedEv)
		seenIDs[verifiedEv.ID] = struct{}{}
	}
	return evs, nil
}

func verifyEvent(ctx context.Context, ev *types.Event, serverName string, keyStore *KeyStore) (error, error) {
	if _, err := spec.NewRoomID(ev.RoomID.String()); err != nil {
		return err, nil
	} else if err := CheckEventSize(ev); err != nil {
		return err, nil
	}
	if ev.Type.Type == spec.MRoomMember && ev.StateKey == nil {
		return errors.New("membership event has no state key"), nil
	}
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
		return errors.New("event ID is not reference hash"), nil
	}

	var verifyErr error
	if serverName == "" {
		verifyErr = gomatrixserverlib.VerifyEventSignatures(ctx, ev.PDU(), keyStore,
			func(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
				return spec.NewUserID(string(senderID), true)
			})
	} else {
		roomVersion, err := gomatrixserverlib.GetRoomVersion(ev.GetRoomVersion())
		if err != nil {
			return nil, err
		}
		results, err := keyStore.VerifyJSONs(ctx, []gomatrixserverlib.VerifyJSONRequest{{
			ServerName:           spec.ServerName(serverName),
			AtTS:                 ev.PDU().OriginServerTS(),
			Message:              b,
			ValidityCheckingFunc: roomVersion.SignatureValidityCheck,
		}})
		if err != nil {
			return nil, err
		}
		verifyErr = results[0].Error
	}
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
	if err := CheckEventSize(ev); err != nil {
		return err
	} else if err := ev.MustGetRoomSpec().CheckCanonicalJSON(ev.PDU().JSON()); err != nil {
		return &gomatrixserverlib.EventValidationError{Code: 400, Message: err.Error()}
	}
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
	ev.ID = refHash

	return nil
}
