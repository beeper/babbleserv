Add push rules to the accounts database

- store Matrix push rules in a new directory in the accounts database, keys:
    - userPushRules (userID, groupName, kind, ruleID) -> partial mautrix.PushRule
        - kind is one of: override, underride, sender, room, content
    - userPushVersions (userID) -> versionstamp of last written rule
- new methods:
    - AccountsDatabase.GetRulesForUser
    - AccountsDatabase.GetRuleForUser
    - AccountsDatabase.PutRuleForUser
    - AccountsDatabase.DeleteRuleForUser
- sync must return all the users push rules if the userPushVersion > the sync token as m.push_rules account data event

Explore the codebase and come up with a plan to implement the above changes.

... implemented, second prompt:

Now we need to evaluate the push rules during event sending.

- add databases.SendLocalEvents which calls rooms.SendLocalEvents
    - modify rooms.SendLocalEvents to take userid -> pushrules map
    - databases.SendLocalEvents fetches local users in room -> makes the map
    - rooms.SendLocalEvents then uses push rules for evaluation
- same for databases.SendFederatedEvents -> rooms.SendFederatedEvents

Explore the codebase and come up with a plan to implement.

... implemented, second prompt:

Now we need to implement Matrix pushers APIs:

- store Matrix pushers (mautrix pushgateway.Pusher) for users in UsersDirectory
    - userPushers subspace (userID, pushKey) -> pushgateway.Pusher
    - methods:
        - AccountsDatabase.GetPushersForUser
        - AccountsDatabase.SetPusherForUser

Explore the codebase with a few agents (databases, routes) and come up with a plan to implement.

... implemented, second prompt:

Finally, now that we've implemented the various push components, let's actually send some push notifications!

- we're going to base this on the CompactNotificationIterator, which is currently disabled
- to prevent the notifications keyspace growing indefinitely (UsersDirectory.notificationVersions), we add a configurable limit to the number of notifications per user/room to keep, this worker will handle deleting the oldest N to maintain the limit (this means read receipt accuracy over the most recent X events per room)
- let's call it PushNotificationIterator, it now has two responsibilities:
    - as events come in, send pushes as required
    - remove old notification count keys
- to implement this, for every event that comes in:
    - fetch all the local users in the room
    - for each user, fetch push notification keys up to (including) the event version
        - if notification was generated for this event (ie userid/roomid/eventversion notification exists), fetch users pushers and send it to each in parallel
    - delete oldest push notifications > the configurable limit

Explore the codebase with a few agents (databases, routes) and come up with a plan to implement.
