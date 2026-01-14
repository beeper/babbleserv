Add notification counting for events

- spec states "The updated notification count from a new event MUST appear in the same /sync response as the event itself."
- store as rooms.users.notificationVersions (userid, roomid, version) -> types.Notifications{notifs, highlights, ...}
- store types.Notifications as msgpack with single letter keys
- for every event send we must (SendLocalEvents, SendFederatedEvents):
    - get local users in the room
    - get all their push rules (stub these for now), before any write txn
    - inside the txn for each event, eval each users rules, map eventsToUserNotifications[id.EventID]types.Notifications{}
    - pass to txnStoreEvents, we apply notificationVersions (userid, roomid, eventVersion) -> types.Notifications{}
- version is the event version (so can get back to the eventid)
- on sync, just
    - range notificationVersions (userid, roomid) => sum counts
- on receipt just
    - clearrange up to (userid, roomid, eventVersionFromReceipt)

Explore the codebase and come up with a plan to implement the above changes.

... implemented, second prompt:

Now we need to implement an EventNotificationIterator to compact notificationVersions:

- compact notificationVersions by aggregating old -> new (userid, roomid, version)
- just iter events constantly compact
    - so should just be merging 2 -> 1 constantly, unless falls behind
- note: in future this worker will also actually turn each (unaggregated) notification into an actual notification for each of the users configured push targets

Explore the codebase and come up with a plan to implement the above changes.

... implemented, third prompt

Let's extend notification counts to handle threads. We need to:

- add ThreadID to types.Notifications (already done)
- in eventsend.go, we:
    - move the notification generation into a new read txn, just before each write txn
    - include threadID in generated notifications, this is:
        - "" if event has no relation
        - $event_id of thread root (found by walking thread relations of m.thread type until no more)
- we need two ways to sum notifications:
    - the current one is fine for non-threading clients
    - new sum by threadID version
- update sync
    - add SyncOption to enable threaded notification counts
    - when set, sum by threadID and update sync response accordingly
    
Explore the codebase and come up with a plan to implement the above changes.
