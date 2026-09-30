package client

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"maunium.net/go/mautrix"
	maufederation "maunium.net/go/mautrix/federation"

	"github.com/beeper/babbleserv/internal/middleware"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type setRoomDirectoryVisibilityRequest struct {
	Visibility string `json:"visibility"`
}

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

// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3directorylistroomroomid
func (c *ClientRoutes) GetRoomDirectoryVisibility(w http.ResponseWriter, r *http.Request) {
	published, err := c.db.Rooms.GetRoomPublished(r.Context(), util.RoomIDFromRequestURLParam(r, "roomID"))
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	if published == nil {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	}
	visibility := "private"
	if *published {
		visibility = "public"
	}
	util.ResponseJSON(w, r, http.StatusOK, map[string]string{"visibility": visibility})
}

// https://spec.matrix.org/v1.16/client-server-api/#put_matrixclientv3directorylistroomroomid
func (c *ClientRoutes) PutRoomDirectoryVisibility(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[*setRoomDirectoryVisibilityRequest](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	if req == nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	if req.Visibility == "" {
		req.Visibility = "public"
	}
	if req.Visibility != "public" && req.Visibility != "private" {
		util.ResponseErrorMessageJSON(w, r, mautrix.MInvalidParam, "Visibility must be public or private")
		return
	}
	err := c.db.Rooms.SetRoomPublished(
		r.Context(), util.RoomIDFromRequestURLParam(r, "roomID"),
		middleware.GetRequestUserID(r), req.Visibility == "public",
	)
	if errors.Is(err, types.ErrRoomNotFound) {
		util.ResponseErrorJSON(w, r, mautrix.MNotFound)
		return
	} else if errors.Is(err, types.ErrRoomPublicationForbidden) {
		util.ResponseErrorJSON(w, r, mautrix.MForbidden)
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, util.EmptyJSON)
}

// https://spec.matrix.org/v1.16/client-server-api/#get_matrixclientv3publicrooms
func (c *ClientRoutes) GetPublicRooms(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		var err error
		limit, err = strconv.Atoi(rawLimit)
		if err != nil || limit < 0 {
			util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
			return
		}
	}
	server := r.URL.Query().Get("server")
	if !c.isLocalPublicRoomsServer(server) {
		query := make(url.Values)
		if limit > 0 {
			query.Set("limit", strconv.Itoa(limit))
		}
		if since := r.URL.Query().Get("since"); since != "" {
			query.Set("since", since)
		}
		middleware.RequireUserAuth(func(w http.ResponseWriter, r *http.Request) {
			c.respondRemotePublicRooms(w, r, server, http.MethodGet, query, nil)
		})(w, r)
		return
	}
	c.respondPublicRooms(w, r, limit, r.URL.Query().Get("since"), types.PublicRoomsFilter{})
}

// https://spec.matrix.org/v1.16/client-server-api/#post_matrixclientv3publicrooms
func (c *ClientRoutes) PostPublicRooms(w http.ResponseWriter, r *http.Request) {
	req, respErr := util.ParseRequestJSON[*publicRoomsRequest](r)
	if respErr != nil {
		util.ResponseErrorJSON(w, r, *respErr)
		return
	}
	if req == nil {
		util.ResponseErrorJSON(w, r, mautrix.MBadJSON)
		return
	}
	if req.Limit < 0 || (req.IncludeAllNetworks && req.ThirdPartyInstanceID != "") {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	}
	server := r.URL.Query().Get("server")
	if !c.isLocalPublicRoomsServer(server) {
		c.respondRemotePublicRooms(w, r, server, http.MethodPost, nil, req)
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
	c.respondPublicRooms(w, r, req.Limit, req.Since, filter)
}

func (c *ClientRoutes) isLocalPublicRoomsServer(server string) bool {
	return server == "" || server == c.config.ServerName
}

func (c *ClientRoutes) respondRemotePublicRooms(
	w http.ResponseWriter,
	r *http.Request,
	server string,
	method string,
	query url.Values,
	request any,
) {
	response := &types.PublicRoomsResponse{}
	_, _, err := c.fedClient.MakeFullRequest(r.Context(), maufederation.RequestParams{
		ServerName:   server,
		Method:       method,
		Path:         maufederation.URLPath{"v1", "publicRooms"},
		Query:        query,
		Authenticate: true,
		RequestJSON:  request,
		ResponseJSON: response,
	})
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, response)
}

func (c *ClientRoutes) respondPublicRooms(
	w http.ResponseWriter,
	r *http.Request,
	limit int,
	since string,
	filter types.PublicRoomsFilter,
) {
	response, err := c.db.Rooms.ListPublicRooms(r.Context(), limit, since, filter)
	if errors.Is(err, types.ErrInvalidPaginationToken) {
		util.ResponseErrorJSON(w, r, mautrix.MInvalidParam)
		return
	} else if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}
	util.ResponseJSON(w, r, http.StatusOK, response)
}
