# Matrix Spec Compatibility

This document explores Babbleserv's compatability (or not) with the Matrix specification. All subject to change.

## No Server Bundled Aggregations

These seem incredibly expensive to calculate for little benefit - clients must still implement all of their own aggregation logic because servers cannot guarantee their own aggregations are correct [citation needed]. So what's the point.

Note: backfilling still presents an issue here, but the `/reations` and threads APIs _will be_ supported and are more suitable for gathering this information.

- see: [MSC2675 limitations](https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/2675-aggregations-server.md#limitations), also see [MSC2677 (reactions) explicitly states server should NOT aggregate](https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/2677-reactions.md#server-side-aggregation-of-mannotation-relationships), despite MSC2575 recommending this exact thing
- note that this also means edits are not applied by the server, clients should (and do) handle these appropriately - from the server perspective events are immutable unless redacted

## Backfill of Federated Rooms

Babbleserv does not currently support backfill over federation.

- rooms will only contain events (excluding state) from the point of joining
- paginating back in these rooms will return no events
    - this can be relatively easily fixed without persisting backfilled events
    - persistence can also be fixed but it's complicated
    
How persistence could work:

- events currently append to a room by the FoundationDB versionstamp, which is similar to a sequence in Postgres
- we could have `rooms/forward-version/<versionstamp>` and `rooms/backward-version/<versionstamp>`
    - new events append to forward as normal
    - backfilled events append to backward
    - chronologically events go:
        - backward-latest-versionstamp -> backward-earliest-versionstamp -> forward-easliest-versionstamp -> forward-latest-versionstamp
    - but this breaks backwards state events, which are part of the forward versionstamp prefix
    - also backfilling old state events

Perhaps a bigger question is - what's the point? If we can do ad-hoc back pagination direct from other homeservers, and anything from the point of join is persisted, is there really much reason to complicate the data model significantly?

Cheap alternative:

- keep the backwards versionstamps as above, but don't bother worrying about state, just backfill events against the earliest state we have in the forward versionstamps for the room (ie the state we joined the room at).
    - but then, why not just backfill over federation without persistence, only benefit is if the other HS was to explode

## Linearized Matrix

See [MSC3995](https://github.com/matrix-org/matrix-spec-proposals/pull/3995) - Babbleserv's data model means that within the local database state is always resolved before storage. There may be multiple dangling events in a room but the current state is always a resolved state in those cases. As such in many ways Babbleserv is similar to linearized Matrix hub servers. Events will be synced in version order, always.

## No Reactions in Relations API

The `/relations` API will not return `m.annotation` evens unless the `rel_type` is explicitly specified (and only `m.annotation` events are returned).

## Profile Updates and Device List Changes are Asynchronous

Request to update/change will return before the changes are applied. Does this even deviate from the spec?

## Push Rules/Notifications/Notification Counts

Not implemented yet.

## Device last_seen and last_seen_ip aren't populated

Not implemented yet.
