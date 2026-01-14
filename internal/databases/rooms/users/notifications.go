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

// TxnCompactNotifications reads all notification entries for a user+room,
// and if there are 2+ entries with the same ThreadID, compacts them into a single entry.
// Notifications with different ThreadIDs are NOT merged together.
// Returns true if any compaction occurred.
func (u *UsersDirectory) TxnCompactNotifications(
	txn fdb.Transaction,
	userID id.UserID,
	roomID id.RoomID,
	upToVersion tuple.Versionstamp,
) bool {
	rng := u.rangeForNotifications(userID, roomID, upToVersion)
	iter := txn.GetRange(rng, fdb.RangeOptions{
		Mode: fdb.StreamingModeWantAll,
	}).Iterator()

	// Group entries by ThreadID
	type entryWithVersion struct {
		kv      fdb.KeyValue
		version tuple.Versionstamp
	}
	byThread := make(map[string][]entryWithVersion)

	for iter.Advance() {
		kv := iter.MustGet()
		notif := types.BytesToNotifications(kv.Value)

		// Extract versionstamp from key
		tup, err := u.notificationVersions.Unpack(kv.Key)
		if err != nil {
			panic(err)
		}
		version := tup[2].(tuple.Versionstamp)

		byThread[notif.ThreadID] = append(byThread[notif.ThreadID], entryWithVersion{kv, version})
	}

	var compacted bool
	for threadID, entries := range byThread {
		// Nothing to compact if 0 or 1 entries for this thread
		if len(entries) < 2 {
			continue
		}

		// Sum all notification counts and find the latest versionstamp for this thread
		var totalNotif types.Notifications
		totalNotif.ThreadID = threadID
		var latestVersion tuple.Versionstamp

		for _, entry := range entries {
			notif := types.BytesToNotifications(entry.kv.Value)
			totalNotif.Count += notif.Count
			totalNotif.Highlight += notif.Highlight

			if types.VersionIsAfter(entry.version, latestVersion) {
				latestVersion = entry.version
			}
		}

		// Clear all entries for this thread
		for _, entry := range entries {
			txn.Clear(entry.kv.Key)
		}

		// Write single compacted entry at the latest versionstamp
		txn.Set(
			u.keyForNotificationVersion(userID, roomID, latestVersion),
			types.NotificationsToBytes(totalNotif),
		)

		compacted = true
	}

	return compacted
}
