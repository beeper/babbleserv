package client

import (
	"net/http"
	"strings"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func makeMembershipContent(membership event.Membership, reason string) map[string]any {
	content := map[string]any{
		"membership": membership,
	}
	if reason != "" {
		content["reason"] = reason
	}
	return content
}

func getOtherServers(r *http.Request, roomID id.RoomID) []string {
	// User provided ?server_name query
	otherServers := r.URL.Query()["server_name"]

	// TODO: don't blindly use roomID server here - but use what? Alias?
	// TODO: use invite sender!
	roomIDBits := strings.SplitN(roomID.String(), ":", 2)
	otherServers = append(otherServers, roomIDBits[len(roomIDBits)-1])
	return otherServers

}

func (c *ClientRoutes) getRoomIDFromRequest(r *http.Request, param string) id.RoomID {
	roomID := util.RoomIDFromRequestURLParam(r, param)
	if strings.HasPrefix(roomID.String(), "!") {
		return roomID
	}

	roomAlias := util.RoomAliasFromRequestURLParam(r, param)
	// Replace #name:server -> @name:server to extract the homeserver
	otherServer := id.UserID("@" + roomAlias[1:]).Homeserver()
	aliasResp, err := c.fclient.LookupRoomAlias(
		r.Context(),
		spec.ServerName(c.config.ServerName),
		spec.ServerName(otherServer),
		string(roomAlias),
	)
	if err != nil {
		return ""
	}

	return id.RoomID(aliasResp.RoomID)
}

type federatedMakeResp struct {
	Event       gomatrixserverlib.ProtoEvent
	RoomVersion gomatrixserverlib.RoomVersion
}

func (c *ClientRoutes) makeFederatedEvent(
	r *http.Request,
	roomID id.RoomID,
	getMakeResp func(string) (federatedMakeResp, error),
) (*types.Event, string, error) {
	otherServers := getOtherServers(r, roomID)

	var err error
	var otherServer string
	var resp federatedMakeResp

	log := hlog.FromRequest(r)

	// We're not in the room - we need to do the join dance to get the room
	// current state from one of the remote servers.
	for _, otherServer = range otherServers {
		log.Debug().
			Str("room_id", roomID.String()).
			Str("server", otherServer).
			Msg("Attempting to make join via server")

		resp, err = getMakeResp(otherServer)

		if err != nil {
			log.Warn().Err(err).
				Str("server", otherServer).
				Msg("Failed to make join via server")
			continue
		}
		break
	}
	if err != nil {
		return nil, "", err
	}

	log.Debug().
		Str("room_id", roomID.String()).
		Str("server", otherServer).
		Msg("Joining room via server")

	roomVersion := string(resp.RoomVersion)

	ev := types.EventFromProtoEvent(resp.Event)
	ev.Timestamp = time.Now().UTC().UnixMilli()
	ev.Origin = c.config.ServerName
	ev.RoomVersion = roomVersion

	keyID, key := c.config.MustGetActiveSigningKey()
	if err := util.HashAndSignEvent(ev, c.config.ServerName, keyID, key); err != nil {
		return nil, "", err
	}

	return ev, otherServer, nil
}
