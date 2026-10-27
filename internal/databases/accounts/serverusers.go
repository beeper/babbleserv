package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

// ErrServerUserTaken refuses a server-owned user whose localpart a registered user has
var ErrServerUserTaken = errors.New("a registered user has the server-owned user's localpart")

// EnsureServerUser creates a local user the server owns, such as BabbleBot, with no password and
// the display name, unless it exists already. Reports whether it created the user. A registered
// user with the localpart is never taken over: that fails with ErrServerUserTaken.
func (a *AccountsDatabase) EnsureServerUser(ctx context.Context, localpart, displayName string) (bool, error) {
	user := types.User{Username: localpart, ServerName: a.config.ServerName, CreatedAt: time.Now().UTC(), ServerOwned: true}
	userID := user.UserID()
	return util.DoWriteTransaction(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		if existing, err := a.users.TxnGetLocalUser(txn, userID); err != nil {
			return false, err
		} else if existing != nil && !existing.ServerOwned {
			return false, fmt.Errorf("%w: %s", ErrServerUserTaken, userID)
		} else if existing != nil {
			return false, nil
		} else if err := a.users.TxnCreateLocalUser(txn, &user, nil); err != nil {
			return false, err
		}
		return true, a.users.TxnStoreUserProfile(txn, userID, &types.UserProfile{DisplayName: displayName})
	})
}

// isReservedLocalpart reports whether registration refuses a localpart, kept for a server-owned user
func (a *AccountsDatabase) isReservedLocalpart(username string) bool {
	return strings.EqualFold(username, a.config.BabbleBot.Localpart)
}

// AddDirectRoom adds a room with another user to the user's m.direct account data
func (a *AccountsDatabase) AddDirectRoom(ctx context.Context, userID, otherUserID id.UserID, roomID id.RoomID) error {
	tup := types.AccountDataTup{UserID: userID, Type: event.AccountDataDirectChats}
	changed, err := util.DoWriteTransactionWithVersion(ctx, a.db, func(txn fdb.Transaction) (bool, error) {
		direct := make(map[id.UserID][]id.RoomID)
		if ad, err := a.txnGetAccountData(txn, tup); err != nil {
			return false, err
		} else if ad != nil {
			if err := json.Unmarshal(ad.Content, &direct); err != nil {
				return false, fmt.Errorf("decode m.direct content: %w", err)
			}
		}
		if slices.Contains(direct[otherUserID], roomID) {
			return false, nil
		}
		direct[otherUserID] = append(direct[otherUserID], roomID)
		content, err := json.Marshal(direct)
		if err != nil {
			return false, err
		}
		a.txnSetAccountData(txn, &types.AccountData{AccountDataTup: tup, Content: content}, tuple.IncompleteVersionstamp(0))
		return true, nil
	})
	if err == nil && changed {
		a.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{userID}})
	}
	return err
}
