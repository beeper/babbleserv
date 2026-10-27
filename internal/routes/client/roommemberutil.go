package client

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog/hlog"
	"maunium.net/go/mautrix"
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
	// A domainless room ID, as v12 rooms have, names no server
	if _, server, found := strings.Cut(roomID.String(), ":"); found {
		otherServers = append(otherServers, server)
	}
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

var (
	// A server answering make_* with a template this server cannot sign
	errInvalidMembershipTemplate = errors.New("invalid membership event template")
	// A server answering make_* for a room of a version this server does not join
	errIncompatibleRoomVersion = errors.New("room version not supported")
)

// makeFederatedEvent signs the template of the first of the other servers giving one it can. When
// none does, an errIncompatibleRoomVersion is returned if a server gave a room version this server
// does not join, else the error of the last server whose request failed, or else an
// errInvalidMembershipTemplate.
func (c *ClientRoutes) makeFederatedEvent(
	r *http.Request,
	roomID id.RoomID,
	membership event.Membership,
	otherServers []string,
	getMakeResp func(string) (federatedMakeResp, error),
) (*types.Event, string, error) {
	log := hlog.FromRequest(r)

	var versionErr, requestErr, templateErr error
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
		} else if !c.config.JoinsRoomVersion(string(resp.RoomVersion)) {
			serverLog.Warn().Str("room_version", string(resp.RoomVersion)).Msg("Server gave a template of a room version not supported")
			versionErr = fmt.Errorf("%w: %s", errIncompatibleRoomVersion, resp.RoomVersion)
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
	return nil, "", cmp.Or(versionErr, requestErr, templateErr)
}

func membershipTemplateEvent(
	userID id.UserID,
	roomID id.RoomID,
	membership event.Membership,
	resp federatedMakeResp,
) (*types.Event, error) {
	return util.MembershipTemplateEvent(userID, roomID, membership, resp.Event, resp.RoomVersion)
}

// As Synapse does, a server failing to give a usable template is a bad gateway
func responseMakeFederatedEventError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errIncompatibleRoomVersion) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MIncompatibleRoomVersion, err.Error())
		return
	} else if errors.Is(err, errInvalidMembershipTemplate) {
		util.ResponseJSON(w, r, http.StatusBadGateway, map[string]string{
			"errcode": util.MUnknown.ErrCode,
			"error":   err.Error(),
		})
		return
	}
	util.ResponseErrorUnknownJSON(w, r, err)
}
