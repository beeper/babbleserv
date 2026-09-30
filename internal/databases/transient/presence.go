package transient

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/transient/presence"
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
	activityAt := presence.LastActive.UTC()
	if presence.LastActive.IsZero() {
		activityAt = time.Now().UTC()
	}
	var timeout time.Time
	if presence.Presence == event.PresenceOnline && userID.Homeserver() == t.config.ServerName {
		// Start tracking timeout for local users (who are online)
		timeout = activityAt.Add(t.config.Transient.PresenceTimeout)
	}
	changed := false
	_, err := util.DoWriteTransactionWithVersion(ctx, t.db, func(txn fdb.Transaction) (types.Nil, error) {
		// An ambiguous commit may retry as a duplicate.
		changed = t.presence.TxnStorePresence(
			txn, userID, presence, timeout, checkMessage, activityAt,
		) || changed
		return nil, nil
	})
	if err != nil {
		return err
	}
	if changed {
		t.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{userID}})
		zerolog.Ctx(ctx).Debug().Msg("Updated user presence")
	} else {
		zerolog.Ctx(ctx).Debug().Msg("Received duplicate presence, updated last active")
	}
	return err
}

func (t *TransientDatabase) GetPresenceTimeouts(
	ctx context.Context,
	toTimeout time.Time,
	limit int,
) ([]presence.Timeout, error) {
	return util.DoReadTransaction(ctx, t.db, func(txn fdb.ReadTransaction) ([]presence.Timeout, error) {
		return t.presence.TxnGetPresenceTimeouts(txn, toTimeout, limit), nil
	})
}

func (t *TransientDatabase) HandlePresenceTimeout(
	ctx context.Context,
	timeout presence.Timeout,
	now time.Time,
) error {
	changed := false
	_, err := util.DoWriteTransactionWithVersion(ctx, t.db, func(txn fdb.Transaction) (types.Nil, error) {
		// An ambiguous commit may retry after the timeout was cleared.
		changed = t.presence.TxnHandlePresenceTimeout(
			txn, timeout, now, t.config.Transient.PresenceTimeout,
		) || changed
		return nil, nil
	})
	if err == nil && changed {
		t.notifier.SendChange(notifier.Change{UserIDs: []id.UserID{timeout.UserID}})
	}
	return err
}
