package users

import (
	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// Notification versions (id.UserID, id.RoomID, tuple.Versionstamp) -> types.Notifications
//

func (u *UsersDirectory) keyForNotificationVersion(
	userID id.UserID,
	roomID id.RoomID,
	version tuple.Versionstamp,
) fdb.Key {
	tup := tuple.Tuple{userID.String(), roomID.String(), version}
	if types.IsIncompleteVersionstamp(version) {
		key, err := u.notificationVersions.PackWithVersionstamp(tup)
		if err != nil {
			panic(err)
		}
		return key
	}
	return u.notificationVersions.Pack(tup)
}

func (u *UsersDirectory) rangeForNotifications(
	userID id.UserID,
	roomID id.RoomID,
	upToVersion tuple.Versionstamp,
) fdb.ExactRange {
	return types.GetVersionRange(
		u.notificationVersions,
		types.ZeroVersionstamp,
		upToVersion,
		userID.String(),
		roomID.String(),
	)
}

// TxnStoreNotification stores a notification delta for an event.
func (u *UsersDirectory) TxnStoreNotification(
	txn fdb.Transaction,
	userID id.UserID,
	roomID id.RoomID,
	version tuple.Versionstamp,
	notif types.Notifications,
) {
	if notif.IsEmpty() {
		return
	}
	txn.SetVersionstampedKey(
		u.keyForNotificationVersion(userID, roomID, version),
		types.NotificationsToBytes(notif),
	)
}

// TxnClearNotificationsUpTo clears all notification entries up to and including the given version.
// Used when a read receipt is received to mark messages as read.
// This clears ALL notifications regardless of ThreadID.
func (u *UsersDirectory) TxnClearNotificationsUpTo(
	txn fdb.Transaction,
	userID id.UserID,
	roomID id.RoomID,
	upToVersion tuple.Versionstamp,
) {
	txn.ClearRange(u.rangeForNotifications(userID, roomID, upToVersion))
}

// TxnClearThreadNotificationsUpTo clears notification entries for a specific thread
// up to and including the given version. Used when a thread-specific read receipt is received.
func (u *UsersDirectory) TxnClearThreadNotificationsUpTo(
	txn fdb.Transaction,
	userID id.UserID,
	roomID id.RoomID,
	threadID string,
	upToVersion tuple.Versionstamp,
) {
	// We need to iterate and selectively clear only notifications matching the threadID
	iter := txn.GetRange(
		u.rangeForNotifications(userID, roomID, upToVersion),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	for iter.Advance() {
		kv := iter.MustGet()
		notif := types.BytesToNotifications(kv.Value)
		if notif.ThreadID == threadID {
			txn.Clear(kv.Key)
		} else if threadID == "main" && notif.ThreadID == "" {
			// Receipts with "main" threadID also clear unthreaded receipts
			txn.Clear(kv.Key)
		}
	}
}

// TxnSumNotifications sums all notification deltas for a user in a room up to
// and including upToVersion. Returns the total notification count and highlight count.
// This sums ALL notifications regardless of ThreadID (for non-threading clients).
func (u *UsersDirectory) TxnSumNotifications(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
	upToVersion tuple.Versionstamp,
) (notifCount int, highlightCount int) {
	iter := txn.GetRange(
		u.rangeForNotifications(userID, roomID, upToVersion),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	for iter.Advance() {
		kv := iter.MustGet()
		notif := types.BytesToNotifications(kv.Value)
		notifCount += notif.Count
		highlightCount += notif.Highlight
	}

	return notifCount, highlightCount
}

// TxnSumNotificationsByThread sums notification deltas grouped by ThreadID.
// Returns:
// - mainNotifCount, mainHighlightCount: notifications with empty ThreadID (main timeline)
// - threadCounts: map of threadID -> notification counts for each thread
func (u *UsersDirectory) TxnSumNotificationsByThread(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
	upToVersion tuple.Versionstamp,
) (mainNotifCount, mainHighlightCount int, threadCounts map[string]*types.UnreadNotificationCounts) {
	threadCounts = make(map[string]*types.UnreadNotificationCounts)

	iter := txn.GetRange(
		u.rangeForNotifications(userID, roomID, upToVersion),
		fdb.RangeOptions{
			Mode: fdb.StreamingModeWantAll,
		},
	).Iterator()

	for iter.Advance() {
		kv := iter.MustGet()
		notif := types.BytesToNotifications(kv.Value)

		if notif.ThreadID == "" {
			mainNotifCount += notif.Count
			mainHighlightCount += notif.Highlight
		} else {
			tc := threadCounts[notif.ThreadID]
			if tc == nil {
				tc = &types.UnreadNotificationCounts{}
				threadCounts[notif.ThreadID] = tc
			}
			tc.NotificationCount += notif.Count
			tc.HighlightCount += notif.Highlight
		}
	}

	return mainNotifCount, mainHighlightCount, threadCounts
}

// TxnGetNotificationAtVersion gets a notification entry at an exact version.
// Returns nil if no notification exists at that version.
func (u *UsersDirectory) TxnGetNotificationAtVersion(
	txn fdb.ReadTransaction,
	userID id.UserID,
	roomID id.RoomID,
	version tuple.Versionstamp,
) *types.Notifications {
	key := u.notificationVersions.Pack(tuple.Tuple{userID.String(), roomID.String(), version})
	b := txn.Get(key).MustGet()
	if b == nil {
		return nil
	}
	notif := types.BytesToNotifications(b)
	return &notif
}

// Clear the oldest notifications per thread to a given limit
func (u *UsersDirectory) TxnCompactNotifications(txn fdb.Transaction, userID id.UserID, roomID id.RoomID, limitPerThread int) int {
	rng := u.rangeForNotifications(userID, roomID, types.ZeroVersionstamp)
	iter := txn.GetRange(rng, fdb.RangeOptions{
		Mode: fdb.StreamingModeWantAll,
	}).Iterator()

	// Generate thread ID -> ordered list of notifications
	byThread := make(map[string][]fdb.KeyValue)

	for iter.Advance() {
		kv := iter.MustGet()
		notif := types.BytesToNotifications(kv.Value)

		if _, ok := byThread[notif.ThreadID]; !ok {
			byThread[notif.ThreadID] = make([]fdb.KeyValue, 0, 1)
		}
		byThread[notif.ThreadID] = append(byThread[notif.ThreadID], kv)
	}

	var deleted int
	for _, keys := range byThread {
		// We need to clear the first N to keep the total as configured
		toDelete := len(keys) - limitPerThread
		if toDelete < 1 {
			continue
		}
		for _, kv := range keys[:toDelete] {
			txn.Clear(kv.Key)
		}
		deleted += toDelete
	}

	return deleted
}
