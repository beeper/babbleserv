package client

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/hlog"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/middleware"
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

// A server answering make_* with a template this server cannot sign
var errInvalidMembershipTemplate = errors.New("invalid membership event template")

// makeFederatedEvent signs the template of the first of the other servers giving one it can. When
// none does, the error of the last server whose request failed is returned, or else an
// errInvalidMembershipTemplate.
func (c *ClientRoutes) makeFederatedEvent(
	r *http.Request,
	roomID id.RoomID,
	membership event.Membership,
	otherServers []string,
	getMakeResp func(string) (federatedMakeResp, error),
) (*types.Event, string, error) {
	log := hlog.FromRequest(r)

	var requestErr, templateErr error
	for _, otherServer := range otherServers {
		serverLog := log.With().
			Stringer("room_id", roomID).
			Str("server", otherServer).
			Logger()
		serverLog.Debug().Msg("Attempting to make federated event via server")

		resp, err := getMakeResp(otherServer)
		if err != nil {
			serverLog.Warn().Err(err).Msg("Failed to make federated event via server")
			requestErr = err
			continue
		}
		ev, err := membershipTemplateEvent(middleware.GetRequestUserID(r), roomID, membership, resp)
		if err == nil {
			keyID, key := c.config.MustGetActiveSigningKey()
			err = util.HashAndSignEvent(ev, c.config.ServerName, keyID, key)
		}
		if err != nil {
			serverLog.Warn().Err(err).Msg("Server gave an invalid membership event template")
			templateErr = fmt.Errorf("%w from %s: %w", errInvalidMembershipTemplate, otherServer, err)
			continue
		}
		serverLog.Debug().Msg("Making federated event via server")
		return ev, otherServer, nil
	}
	return nil, "", cmp.Or(requestErr, templateErr)
}

// membershipTemplateEvent holds a make_* template to what was asked for and sets the membership
// asked for, as Synapse does
func membershipTemplateEvent(
	userID id.UserID,
	roomID id.RoomID,
	membership event.Membership,
	resp federatedMakeResp,
) (*types.Event, error) {
	if _, err := gomatrixserverlib.GetRoomVersion(resp.RoomVersion); err != nil {
		return nil, err
	}
	ev, err := types.EventFromProtoEvent(resp.Event)
	if err != nil {
		return nil, err
	} else if ev.Type != event.StateMember || ev.StateKey == nil || *ev.StateKey != userID.String() ||
		ev.Sender != userID || ev.RoomID != roomID {
		return nil, fmt.Errorf("not a member event of %s in %s", userID, roomID)
	}
	if !gjson.ParseBytes(ev.Content).IsObject() {
		return nil, errors.New("content is not an object")
	} else if ev.Content, err = sjson.SetBytes(ev.Content, "membership", membership); err != nil {
		return nil, err
	}
	ev.Timestamp = time.Now().UTC().UnixMilli()
	ev.RoomVersion = string(resp.RoomVersion)
	return ev, nil
}

// As Synapse does, a server failing to give a usable template is a bad gateway
func responseMakeFederatedEventError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errInvalidMembershipTemplate) {
		util.ResponseJSON(w, r, http.StatusBadGateway, map[string]string{
			"errcode": util.MUnknown.ErrCode,
			"error":   err.Error(),
		})
		return
	}
	util.ResponseErrorUnknownJSON(w, r, err)
}
