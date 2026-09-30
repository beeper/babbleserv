package federation

import (
	"errors"
	"net/http"
	"strconv"

	"maunium.net/go/mautrix"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type publicRoomsRequest struct {
	Filter               *publicRoomsFilter `json:"filter,omitempty"`
	IncludeAllNetworks   bool               `json:"include_all_networks,omitempty"`
	Limit                int                `json:"limit,omitempty"`
	Since                string             `json:"since,omitempty"`
	ThirdPartyInstanceID string             `json:"third_party_instance_id,omitempty"`
}

type publicRoomsFilter struct {
	GenericSearchTerm string    `json:"generic_search_term,omitempty"`
	RoomTypes         []*string `json:"room_types"`
}

// https://spec.matrix.org/v1.16/server-server-api/#get_matrixfederationv1publicrooms
func (f *FederationRoutes) GetPublicRooms(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		var err error
		limit, err = strconv.Atoi(rawLimit)
		if err != nil || limit < 0 {
			util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
			return
		}
	}
	includeAllNetworks, err := parsePublicRoomsBool(r.URL.Query().Get("include_all_networks"))
	if err != nil {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	}
	f.respondPublicRooms(w, r, publicRoomsRequest{
		IncludeAllNetworks:   includeAllNetworks,
		Limit:                limit,
		Since:                r.URL.Query().Get("since"),
		ThirdPartyInstanceID: r.URL.Query().Get("third_party_instance_id"),
	})
}

// https://spec.matrix.org/v1.16/server-server-api/#post_matrixfederationv1publicrooms
func (f *FederationRoutes) PostPublicRooms(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[*publicRoomsRequest](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	if req == nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	f.respondPublicRooms(w, r, *req)
}

func parsePublicRoomsBool(raw string) (bool, error) {
	switch raw {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New("invalid publicRooms boolean")
	}
}

func (f *FederationRoutes) respondPublicRooms(w http.ResponseWriter, r *http.Request, req publicRoomsRequest) {
	if req.Limit < 0 || (req.IncludeAllNetworks && req.ThirdPartyInstanceID != "") {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	}
	if req.ThirdPartyInstanceID != "" {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}
	filter := types.PublicRoomsFilter{}
	if req.Filter != nil {
		filter.GenericSearchTerm = req.Filter.GenericSearchTerm
		if req.Filter.RoomTypes != nil {
			filter.FilterRoomTypes = true
			filter.RoomTypes = make(map[string]struct{}, len(req.Filter.RoomTypes))
			for _, roomType := range req.Filter.RoomTypes {
				if roomType == nil {
					filter.RoomTypes[""] = struct{}{}
				} else {
					filter.RoomTypes[*roomType] = struct{}{}
				}
			}
		}
	}
	response, err := f.db.Rooms.ListPublicRooms(r.Context(), req.Limit, req.Since, filter)
	if errors.Is(err, types.ErrInvalidPaginationToken) {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, response)
}
