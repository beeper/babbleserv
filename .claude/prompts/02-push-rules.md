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
