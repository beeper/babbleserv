package client

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/matrix-org/gomatrix"
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

// getOtherServers returns the servers asked to join or knock on a room this server is not in, as
// Synapse asks them: those the request names with ?via, or ?server_name which it replaced in spec
// 1.12, those the room alias lookup gave, and the room ID's domain, unless domainless as in v12 rooms
func getOtherServers(r *http.Request, roomID id.RoomID, aliasServers []string) []string {
	query := r.URL.Query()
	otherServers := slices.Concat(query["via"], query["server_name"], aliasServers)
	if _, server, found := strings.Cut(roomID.String(), ":"); found {
		otherServers = append(otherServers, server)
	}
	return otherServers
}

// remoteMembershipServers returns the servers getOtherServers gives, then that of the user's inviter,
// without repeats or this server
func (c *ClientRoutes) remoteMembershipServers(r *http.Request, roomID id.RoomID, userID id.UserID, aliasServers []string) ([]string, error) {
	otherServers := getOtherServers(r, roomID, aliasServers)
	if mtup, ev, err := c.db.Rooms.GetCurrentMembershipAndEvent(r.Context(), userID, roomID); err != nil {
		return nil, err
	} else if mtup != nil && mtup.Membership == event.MembershipInvite && ev != nil {
		otherServers = append(otherServers, ev.Sender.Homeserver())
	}
	servers := make([]string, 0, len(otherServers))
	for _, server := range otherServers {
		if server != "" && server != c.config.ServerName && !slices.Contains(servers, server) {
			servers = append(servers, server)
		}
	}
	return servers, nil
}

// getRoomIDFromRequest returns the room the request names, looking up a room alias over federation,
// with the servers the lookup gave. An alias that cannot be looked up gives no room ID.
func (c *ClientRoutes) getRoomIDFromRequest(r *http.Request, param string) (id.RoomID, []string) {
	roomID := util.RoomIDFromRequestURLParam(r, param)
	if strings.HasPrefix(roomID.String(), "!") {
		return roomID, nil
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
		return "", nil
	}

	servers := []string{otherServer}
	for _, server := range aliasResp.Servers {
		servers = append(servers, string(server))
	}
	return id.RoomID(aliasResp.RoomID), servers
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
	// A make_* request that got no answer from the server
	errMakeRequestFailed = errors.New("make request failed")
	// A remote membership with no server to ask
	errNoKnownServers = errors.New("no known servers")
)

// makeFederatedEvent signs the template of the first of the other servers giving one it can. When
// none does, an errIncompatibleRoomVersion is returned if a server gave a room version this server
// does not join, else an util.ErrRestrictedJoinUnsupported if a server gave a restricted join, else
// the error of the last server whose request failed, or else an errInvalidMembershipTemplate, or
// errNoKnownServers without servers.
func (c *ClientRoutes) makeFederatedEvent(
	r *http.Request,
	roomID id.RoomID,
	membership event.Membership,
	otherServers []string,
	getMakeResp func(string) (federatedMakeResp, error),
) (*types.Event, string, error) {
	log := hlog.FromRequest(r)

	var versionErr, unsupportedErr, requestErr, templateErr error
	for _, otherServer := range otherServers {
		serverLog := log.With().
			Stringer("room_id", roomID).
			Str("server", otherServer).
			Logger()
		serverLog.Debug().Msg("Attempting to make federated event via server")

		resp, err := getMakeResp(otherServer)
		if err != nil {
			serverLog.Warn().Err(err).Msg("Failed to make federated event via server")
			requestErr = fmt.Errorf("%w via %s: %w", errMakeRequestFailed, otherServer, err)
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
			if errors.Is(err, util.ErrRestrictedJoinUnsupported) {
				unsupportedErr = templateErr
			}
			continue
		}
		serverLog.Debug().Msg("Making federated event via server")
		return ev, otherServer, nil
	}
	return nil, "", cmp.Or(versionErr, unsupportedErr, requestErr, templateErr, errNoKnownServers)
}

func membershipTemplateEvent(
	userID id.UserID,
	roomID id.RoomID,
	membership event.Membership,
	resp federatedMakeResp,
) (*types.Event, error) {
	return util.MembershipTemplateEvent(userID, roomID, membership, resp.Event, resp.RoomVersion)
}

// joinedRoomVersions are the room versions this server joins rooms of, which a make_knock offers
func (c *ClientRoutes) joinedRoomVersions() []gomatrixserverlib.RoomVersion {
	var versions []gomatrixserverlib.RoomVersion
	for version := range gomatrixserverlib.RoomVersions() {
		if c.config.JoinsRoomVersion(string(version)) {
			versions = append(versions, version)
		}
	}
	slices.Sort(versions)
	return versions
}

// As Synapse does, a server refusing to give a template is passed on, with the room version of an
// incompatible one, and a server failing to give a usable one is a bad gateway. A restricted join to
// a state DAG room is refused.
func responseMakeFederatedEventError(w http.ResponseWriter, r *http.Request, err error) {
	var httpErr gomatrix.HTTPError
	if errors.As(err, &httpErr) {
		var respErr mautrix.RespError
		if httpErr.Code >= 400 && httpErr.Code < 500 && json.Unmarshal(httpErr.Contents, &respErr) == nil && respErr.ErrCode != "" {
			body := map[string]any{"errcode": respErr.ErrCode, "error": respErr.Err}
			if roomVersion, ok := respErr.ExtraData["room_version"].(string); ok && respErr.ErrCode == mautrix.MIncompatibleRoomVersion.ErrCode {
				body["room_version"] = roomVersion
			}
			util.ResponseJSON(w, r, httpErr.Code, body)
			return
		}
		util.ResponseJSON(w, r, http.StatusBadGateway, map[string]string{
			"errcode": util.MUnknown.ErrCode,
			"error":   err.Error(),
		})
		return
	} else if errors.Is(err, errIncompatibleRoomVersion) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MIncompatibleRoomVersion, err.Error())
		return
	} else if errors.Is(err, errNoKnownServers) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MNotFound, "No known servers")
		return
	} else if errors.Is(err, util.ErrRestrictedJoinUnsupported) {
		util.ResponseErrorMessageJSON(w, r, mautrix.MForbidden, util.ErrRestrictedJoinUnsupported.Error())
		return
	} else if errors.Is(err, errInvalidMembershipTemplate) || errors.Is(err, errMakeRequestFailed) {
		util.ResponseJSON(w, r, http.StatusBadGateway, map[string]string{
			"errcode": util.MUnknown.ErrCode,
			"error":   err.Error(),
		})
		return
	}
	util.ResponseErrorUnknownJSON(w, r, err)
}
