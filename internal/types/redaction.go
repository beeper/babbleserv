package types

import (
	"slices"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// From version 11 a member event's redaction keeps third_party_invite.signed, as Synapse does, which
// gomatrixserverlib's version 9 rules drop, so such events' IDs differed from other servers'.
// Registered over gomatrixserverlib's versions, so its signature checks agree. Only lookups by
// version string see the override: gomatrixserverlib's NewEventFrom*JSON redact with the inner
// rules on a hash mismatch, so remote events mustn't be parsed through them.
var updatedRedactionRuleVersions = []gomatrixserverlib.RoomVersion{
	gomatrixserverlib.RoomVersionV11,
	gomatrixserverlib.RoomVersionV12,
	gomatrixserverlib.RoomVersionStateDAGs,
}

func init() {
	for _, version := range updatedRedactionRuleVersions {
		gomatrixserverlib.SetRoomVersion(updatedRedactionRules{gomatrixserverlib.MustGetRoomVersion(version)})
	}
}

type updatedRedactionRules struct {
	gomatrixserverlib.IRoomVersion
}

func (v updatedRedactionRules) RedactEventJSON(eventJSON []byte) ([]byte, error) {
	redacted, err := v.IRoomVersion.RedactEventJSON(eventJSON)
	if err != nil {
		return nil, err
	}
	thirdPartyInvite := gjson.GetBytes(eventJSON, "content.third_party_invite")
	if gjson.GetBytes(redacted, "type").Str != spec.MRoomMember || !thirdPartyInvite.IsObject() {
		return redacted, nil
	}
	kept := []byte("{}")
	if signed := thirdPartyInvite.Get("signed"); signed.Exists() {
		if kept, err = sjson.SetRawBytes(kept, "signed", []byte(signed.Raw)); err != nil {
			return nil, err
		}
	}
	return sjson.SetRawBytes(redacted, "content.third_party_invite", kept)
}

// RedactedEventID reads content.redacts from version 11, the top-level member before
func (ev *Event) RedactedEventID() id.EventID {
	if ev.Type != event.EventRedaction {
		return ""
	} else if slices.Contains(updatedRedactionRuleVersions, ev.GetRoomVersion()) {
		return id.EventID(gjson.GetBytes(ev.Content, "redacts").Str)
	}
	return ev.Redacts
}
