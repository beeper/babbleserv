package rooms

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

const (
	defaultPublicRoomsLimit = 100
	maxPublicRoomsLimit     = 100
	maxPublicRoomsScan      = 2000
)

func (r *RoomsDatabase) GetRoomPublished(ctx context.Context, roomID id.RoomID) (*bool, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*bool, error) {
		roomBytes := txn.Get(r.KeyForRoom(roomID)).MustGet()
		if roomBytes == nil {
			return nil, nil
		}
		published := types.MustNewRoomFromBytes(roomBytes, roomID).Public
		return &published, nil
	})
}

func (r *RoomsDatabase) SetRoomPublished(
	ctx context.Context,
	roomID id.RoomID,
	userID id.UserID,
	published bool,
) error {
	_, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (types.Nil, error) {
		roomBytes := txn.Get(r.KeyForRoom(roomID)).MustGet()
		if roomBytes == nil {
			return nil, types.ErrRoomNotFound
		}
		room := types.MustNewRoomFromBytes(roomBytes, roomID)
		if !r.users.TxnIsUserJoinedRoom(txn, userID, roomID) {
			return nil, types.ErrRoomPublicationForbidden
		}
		eventsProvider := r.events.NewTxnEventsProvider(ctx, txn)
		createEvent := r.events.TxnGetCurrentRoomStateEvent(txn, roomID, types.StateTup{
			Type: event.StateCreate,
		}, eventsProvider)
		if createEvent == nil || createEvent.Sender != userID {
			return nil, types.ErrRoomPublicationForbidden
		}

		r.txnSetRoomPublished(txn, room, published)
		return nil, nil
	})
	return err
}

func (r *RoomsDatabase) txnSetRoomPublished(txn fdb.Transaction, room *types.Room, published bool) {
	room.Public = published
	if published {
		txn.Set(r.keyForPublishedRoom(room.MemberCount, room.ID), nil)
	} else {
		txn.Clear(r.keyForPublishedRoom(room.MemberCount, room.ID))
	}
	txn.Set(r.KeyForRoom(room.ID), room.ToMsgpack())
}

func (r *RoomsDatabase) ListPublicRooms(
	ctx context.Context,
	limit int,
	since string,
	filter types.PublicRoomsFilter,
) (*types.PublicRoomsResponse, error) {
	if limit == 0 {
		limit = defaultPublicRoomsLimit
	} else if limit < 0 {
		return nil, types.ErrInvalidPaginationToken
	} else if limit > maxPublicRoomsLimit {
		limit = maxPublicRoomsLimit
	}
	direction, cursor, err := decodePublicRoomsToken(since)
	if err != nil {
		return nil, err
	}

	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.PublicRoomsResponse, error) {
		begin, end := r.publishedRooms.FDBRangeKeys()
		keyRange := fdb.KeyRange{Begin: begin.FDBKey(), End: end.FDBKey()}
		reverse := direction == "b"
		if cursor != nil {
			cursorKey := r.publishedRooms.Pack(cursor)
			if reverse {
				keyRange.End = cursorKey
			} else {
				keyRange.Begin = append(append(fdb.Key(nil), cursorKey...), 0x00)
			}
		}

		kvs := txn.GetRange(keyRange, fdb.RangeOptions{
			Limit:   maxPublicRoomsScan + 1,
			Reverse: reverse,
			Mode:    fdb.StreamingModeWantAll,
		}).GetSliceOrPanic()
		response := &types.PublicRoomsResponse{Chunk: make([]*types.PublicRoomInfo, 0, limit)}
		scanLimit := len(kvs)
		moreKeys := false
		if scanLimit > maxPublicRoomsScan {
			scanLimit = maxPublicRoomsScan
			moreKeys = true
		}
		var lastScanned tuple.Tuple
		var roomFutures []fdb.FutureByteSlice
		resultLimitReached := false
		for i, kv := range kvs[:scanLimit] {
			// Issue metadata reads in batches so filtering does not serialize every read.
			if i%100 == 0 {
				batch := kvs[i:min(i+100, scanLimit)]
				roomFutures = make([]fdb.FutureByteSlice, len(batch))
				for j, entry := range batch {
					key, err := r.publishedRooms.Unpack(entry.Key)
					if err != nil {
						panic(err)
					}
					roomFutures[j] = txn.Get(r.KeyForRoom(id.RoomID(key[1].(string))))
				}
			}
			keyTuple, unpackErr := r.publishedRooms.Unpack(kv.Key)
			if unpackErr != nil || len(keyTuple) != 2 {
				continue
			}
			roomID := id.RoomID(keyTuple[1].(string))
			lastScanned = keyTuple
			roomBytes := roomFutures[i%100].MustGet()
			if roomBytes == nil {
				continue
			}
			room := types.MustNewRoomFromBytes(roomBytes, roomID)
			if !room.Public || !publicRoomMatches(room, filter) {
				continue
			}
			response.Chunk = append(response.Chunk, publicRoomInfo(room))
			if len(response.Chunk) > limit {
				moreKeys = true
				response.Chunk = response.Chunk[:limit]
				resultLimitReached = true
				break
			}
		}

		if reverse {
			for left, right := 0, len(response.Chunk)-1; left < right; left, right = left+1, right-1 {
				response.Chunk[left], response.Chunk[right] = response.Chunk[right], response.Chunk[left]
			}
		}
		if len(response.Chunk) > 0 {
			firstRoom := response.Chunk[0]
			lastRoom := response.Chunk[len(response.Chunk)-1]
			first := tuple.Tuple{-int64(firstRoom.NumJoinedMembers), firstRoom.RoomID.String()}
			last := tuple.Tuple{-int64(lastRoom.NumJoinedMembers), lastRoom.RoomID.String()}
			continuation := lastScanned
			if resultLimitReached {
				if reverse {
					continuation = first
				} else {
					continuation = last
				}
			}
			if reverse {
				if moreKeys {
					response.PrevBatch = encodePublicRoomsToken("b", continuation)
				}
				if cursor != nil {
					response.NextBatch = encodePublicRoomsToken("f", last)
				}
			} else {
				if cursor != nil {
					response.PrevBatch = encodePublicRoomsToken("b", first)
				}
				if moreKeys {
					response.NextBatch = encodePublicRoomsToken("f", continuation)
				}
			}
		} else if moreKeys && lastScanned != nil {
			if reverse {
				response.PrevBatch = encodePublicRoomsToken("b", lastScanned)
			} else {
				response.NextBatch = encodePublicRoomsToken("f", lastScanned)
			}
		}
		return response, nil
	})
}

func publicRoomMatches(room *types.Room, filter types.PublicRoomsFilter) bool {
	if filter.FilterRoomTypes {
		if _, ok := filter.RoomTypes[room.Type]; !ok {
			return false
		}
	}
	term := strings.ToLower(filter.GenericSearchTerm)
	if term == "" {
		return true
	}
	return strings.Contains(strings.ToLower(room.Name), term) ||
		strings.Contains(strings.ToLower(room.Topic), term) ||
		strings.Contains(strings.ToLower(room.CanonicalAlias), term) ||
		strings.Contains(strings.ToLower(room.ID.String()), term)
}

func publicRoomInfo(room *types.Room) *types.PublicRoomInfo {
	joinRule := room.JoinRule
	if joinRule == "" {
		joinRule = string(event.JoinRuleInvite)
	}
	return &types.PublicRoomInfo{
		RoomID:           room.ID,
		AvatarURL:        room.AvatarURL,
		CanonicalAlias:   id.RoomAlias(room.CanonicalAlias),
		GuestCanJoin:     room.GuestAccess == string(event.GuestAccessCanJoin),
		JoinRule:         joinRule,
		Name:             room.Name,
		NumJoinedMembers: room.MemberCount,
		RoomType:         room.Type,
		Topic:            room.Topic,
		WorldReadable:    room.HistoryVisibility == string(event.HistoryVisibilityWorldReadable),
	}
}

func encodePublicRoomsToken(direction string, cursor tuple.Tuple) string {
	return base64.RawURLEncoding.EncodeToString(tuple.Tuple{"v1", direction, cursor[0], cursor[1]}.Pack())
}

func decodePublicRoomsToken(token string) (direction string, cursor tuple.Tuple, err error) {
	if token == "" {
		return "f", nil, nil
	}
	if len(token) > 512 {
		return "", nil, types.ErrInvalidPaginationToken
	}
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(token)
	if decodeErr != nil {
		return "", nil, types.ErrInvalidPaginationToken
	}
	value, unpackErr := tuple.Unpack(decoded)
	if unpackErr != nil || len(value) != 4 {
		return "", nil, types.ErrInvalidPaginationToken
	}
	version, versionOK := value[0].(string)
	direction, directionOK := value[1].(string)
	memberCount, countOK := value[2].(int64)
	roomID, roomOK := value[3].(string)
	if !versionOK || !directionOK || !countOK || !roomOK || version != "v1" ||
		(direction != "f" && direction != "b") || memberCount > 0 || !strings.HasPrefix(roomID, "!") {
		return "", nil, types.ErrInvalidPaginationToken
	}
	return direction, tuple.Tuple{memberCount, roomID}, nil
}
