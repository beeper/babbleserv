package transient

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/notifier"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func (t *TransientDatabase) PaginatePresenceChanges(
	ctx context.Context,
	options types.PaginationOptions,
) ([]*types.PresenceChange, error) {
	return util.DoReadTransaction(ctx, t.db, func(txn fdb.ReadTransaction) ([]*types.PresenceChange, error) {
		return t.presence.TxnPaginatePresenceChanges(txn, options), nil
	})
}

func (t *TransientDatabase) GetUserPresence(
	ctx context.Context,
	userID id.UserID,
) (*types.Presence, error) {
	return util.DoReadTransaction(ctx, t.db, func(txn fdb.ReadTransaction) (*types.Presence, error) {
		presence := t.presence.TxnGetPresence(txn, userID)
		if presence != nil {
			lastActive := t.presence.TxnGetLastActive(txn, userID)
			if lastActive != nil {
				presence.LastActive = *lastActive
			}
		}
		return presence, nil
	})
}

func (t *TransientDatabase) UpdateUserPresence(
	ctx context.Context,
	userID id.UserID,
	presence *types.Presence,
) error {
	return t.updateUserPresenceOrState(ctx, userID, presence, true)
}

func (t *TransientDatabase) UpdateUserPresenceState(
	ctx context.Context,
	userID id.UserID,
	state event.Presence,
) error {
	presence := &types.Presence{Presence: state}
	return t.updateUserPresenceOrState(ctx, userID, presence, false)
}

func (t *TransientDatabase) updateUserPresenceOrState(
	ctx context.Context,
	userID id.UserID,
	presence *types.Presence,
	checkMessage bool,
) error {
	var timeout time.Time
	if presence.Presence == event.PresenceOnline && userID.Homeserver() == t.config.ServerName {
		// Start tracking timeout for local users (who are online)
		timeout = time.Now().UTC().Add(t.config.Transient.PresenceTimeout)
	}
	_, err := util.DoWriteTransactionWithVersion(ctx, t.db, func(txn fdb.Transaction) (types.Nil, error) {
		if changed := t.presence.TxnStorePresence(txn, userID, presence, timeout, checkMessage); changed {
			t.notifier.SendChange(notifier.Change{
				UserIDs: []id.UserID{userID},
			})
			zerolog.Ctx(ctx).Debug().Msg("Updated user presence")
		} else {
			zerolog.Ctx(ctx).Debug().Msg("Received duplicate presence, updated last active")
		}
		return nil, nil
	})
	return err
}

func (t *TransientDatabase) SetPresenceTimeout(
	ctx context.Context,
	timeout time.Time,
	userID id.UserID,
) error {
	_, err := util.DoWriteTransaction(ctx, t.db, func(txn fdb.Transaction) (types.Nil, error) {
		t.presence.TxnStorePresenceTimeout(txn, timeout, userID)
		return nil, nil
	})
	return err
}

func (t *TransientDatabase) GetPresenceTimeouts(
	ctx context.Context,
	toTimeout time.Time,
) ([]id.UserID, error) {
	return util.DoReadTransaction(ctx, t.db, func(txn fdb.ReadTransaction) ([]id.UserID, error) {
		return t.presence.TxnGetPresenceTimeouts(txn, toTimeout), nil
	})
}

func (t *TransientDatabase) ClearPresenceTimeouts(
	ctx context.Context,
	toTimeout time.Time,
) error {
	_, err := util.DoWriteTransaction(ctx, t.db, func(txn fdb.Transaction) (types.Nil, error) {
		t.presence.TxnClearPresenceTimeouts(txn, toTimeout)
		return nil, nil
	})
	return err
}
