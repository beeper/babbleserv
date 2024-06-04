# Babbleserv Data Model: Rooms Database

The rooms database is responsible for the majority of the Matrix implementation including: rooms, events, receipts, room aliases, room directory, user directory.

## Tuple Values

The rooms database re-uses a couple of core tuple values in multiple places. Defined in `types/tuples.go`:

### `StateTup`

Consists of event ID, event type, state key.

### `MembershipTup`

Consists of event ID, room ID, membership.

## Directories

### Events Directory

Note all these are under the "events" directory (FDB concept) and will all fall under the "events" prefix.

#### Event Data

```
("by-id", event_id) -> event msgpack bytes
```
- lookup individual events (maps to Babbleserv `types.Event`)
- include `auth_events` & `prev_events`

#### Indices

##### Global version order

```
("by-version", versionstamp) -> (event_id, room_id)
```
- global all events order, allows scanning all events for backfills/similar
- paginate all events while filtering by room ID set
    - alternative is to paginate each room separately

##### Event ID to version
```
("id-to-version", event_id) -> versionstamp
```
- get version of event (so can then lookup state at that point)

##### Room ID global version order
```
("by-room", room_id, versionstamp) -> event_id
```
- paginate room events

##### Room current forward extremities

```
("by-room-extrem", room_id, event_id) -> ""
```
- provides range over DAG extremities
- on local send take all event IDs as `prev_events`
- on store event clear any found in `prev_events`

##### Room state version
```
("by-room-state", room_id, versionstamp) -> StateTup
```
- get room state at/before versionstamp
    - pull everything <= by versionstamp, latest (type, state_key) pairs win
    - means iterating over duplicates, but alternative is to snapshot state at each change which is extremely space wasteful
- get state changes between X and Y events in this room (needs MSC, see [this synapse issue](https://github.com/matrix-org/synapse/issues/13618))

##### Room current state (excluding members)

```
("by-room-current-state", room_id, ev_type, state_key) -> event_id
```
- current state of room
- by key AND "all current state for room excluding members”
- keeps state, excl members, together

##### Room current memberships

```
("by-room-members", room_id, user_id) -> MembershipTup
```
- current membership events in room
- init sync
    - can use membership value to skip fetching leave events
- keeps room memberships together

##### Room specific state type/key versions (includes members)
```
("by-room-version-state", room_id, ev_type, state_key, versionstamp) -> event_id
```
- paginate room state by event type
- only by key since the prefix has many versions per key
- "get m.room.power_levels at version X"
- delete old versions (2nd rev key onwards) where >7 days old

Current indices map current/latest values to event id
These will have contention during many state changes in same room on same keys

##### Room event relations (excluding annotations/reactions)
```
("by-room-event-relation", room_id, rel_to_ev_id, versionstamp) -> (event_id, rel_type)
```
- paginate events relating to this event
- paginate events of a certain rel_type relating to this event
    - have to paginate through types that don't match
    - probably sufficient performance (rare)
    
##### Room event reactions
```
("by-room-event-reaction", room_id, rel_to_ev_id, user_id, key) -> (event_id)
```
- de-dupe reactions to an event by user/key
- paginate reactions for a given event (`/relations/rel_to_ev_id/m.annotation`)

##### Room threads

Note: only set for new thread roots, on the first replying (in-thread) event.

```
("by-room-thread-root", room_id, versionstamp_of_root_event) -> root_event_id
```
- Paginate thread roots in a room


### Receipts Directory

These all live under the "receipts" FDB directory.

```
("by-room", room_id) -> (user_id, event_id, data?)
```


### Rooms Directory

All these live under the "rooms" FDB directory.

#### Room Data

```
("by-id", room_id) -> room msgpack bytes
```

#### Indices

##### Public Rooms

```
("pub-rooms", room_id) -> ''
```
- Add/remove to include rooms in the public directory
- Paginate room directory

##### Room Aliases

```
("room-aliases", alias) -> room_Id
```


### Users Directory

Note all these are under the "users" FDB directory. This is specific to local users, ideally we won't need to store any federated user stuff here.

#### User Data

```
("profiles", user_id) -> user profile msgpack bytes
```
- get `types.UserProfile` objects msgpack encoded

#### Indices

##### User Memberships

```
("memberships", user_id, room_id) -> MembershipTup
```
- get memberships for a user for sync

##### User membership Changes

```
("membership-changes", user_id, versionstamp) -> MembershipTup
```
- get historical membership changes for incremental sync (get now, apply changes since -> now, last per room wins)

##### User outlier memberships (federation invites/knocks)

```
("outlier-memberships", user_id, room_id) -> MembershipTup
```
- get currently pending invites/knocks for a user where the server is not a member of the room


### Servers Directory

Just like the users directory we need to keep track of the currently joined members by server, as well as when this changes over time. This allows us to handle outgoing federation traffic much like sync is handled for users.

#### Server Data

```
("by-name", server_name) -> server msgpack bytes
```
- get `types.Server` objects msgpack encoded

#### Indices

##### Room joined server members

```
("server-joined-members", room_id, server_name, username) -> ''
```
- specifically tracks joined memberships by server/username
- when a nonlocal join membership arrives, store this
- when any nonlocal non-join membership arrives, clear this

##### Server memberships

```
("server-memberships", server_name, room_id) -> MembershipTup
```
- note: only contains joins
- set this if >0 server joined members (see above)

##### Server membership changes

```
("server-membership-changes", server_name, versionstamp) -> MembershipTup
```
- append leave/join membership to this if joined members changes from 0 to nonzero or back
