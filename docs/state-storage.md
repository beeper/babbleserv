# Room state storage

Room state is stored in FoundationDB as immutable maps. An event records the contexts before and after it; the room record points to its current context. Updating state creates new pages and reuses unchanged parts of the old maps.

This document describes the persisted data and the operations on it. [Event sending](event-sending.md) describes how preparation, state resolution, staging and publication use those operations. [Generated schema](data-model-generated.md) lists the database's other subspaces.

## Contexts

A state tuple is `(event_type, state_key)`. A context maps each tuple to the event occupying it at one point in the room DAG. It has two roots:

| Map | Key | Value |
| --- | --- | --- |
| State | Tuple-encoded `(event_type, state_key)`, excluding `m.room.member` | Event ID bytes |
| Members | User ID bytes | One membership-code byte followed by event ID bytes |

The separate member map lets callers read non-member state without traversing all members. Membership is stored alongside its event ID so authorization lookups, state diffs and joined-member counts can inspect it without loading the event body. The codes are join `1`, invite `2`, leave `3`, ban `4` and knock `5`.

Page IDs and context IDs are `types.StateHash`: the first 16 bytes of SHA-256 over the encoded record. A page hashes its stored bytes; a context hashes its two encoded roots. Membership is determined by its event, so the context ID depends on the state event IDs, not the order in which the context was built.

There are two different empty values:

- An all-zero **page ID** means an empty map. Encodings represent it as an empty byte string.
- An all-zero **context ID** means unknown state. Known empty state has its own nonzero ID, `state.EmptyContext`, calculated from two empty roots. It needs no stored context row.

## FoundationDB records

Paths below are logical prefixes relative to the `rooms` FDB directory. The directory layer supplies the actual byte prefixes; keys use FDB tuple encoding. Values are tuples unless another encoding is stated. Hash fields are byte strings, not their hexadecimal display form.

### Contexts, pages and resolution results

```text
state/cx / (room_id, context_id)              -> (state_root, member_root)
state/ob / (room_id, page_id)                 -> encoded leaf or branch
state/rs / (room_id, implementation_version,
            resolution_key)                 -> (result_context_id)
```

Contexts and pages are immutable. Writing the same ID again writes the same bytes. They are scoped by room: two rooms with the same map contents still have separate FDB records.

A resolution key hashes the sorted, distinct input context IDs. `implementation_version` is `stateres.Version`, which must change when an algorithm change can change a result. Artifact jobs write `rs` only after staging the result context and its pages. Small resolutions run in memory without adding an `rs` row.

### Events and state history

```text
events/eid / event_id                    -> Event (msgpack)
events/etv / event_id                    -> versionstamp
events/evr / versionstamp                -> EventTup
events/rmv / (room_id, versionstamp)     -> EventTup
events/elv / (room_id, versionstamp)     -> EventTup
events/rch / (room_id, versionstamp)     -> (context_id)
events/rex / (room_id, event_id)         -> empty
```

`EventTup` is the tuple `(event_id, room_id, sender, event_type)`. Versionstamp values are also tuple-encoded.

The event body holds `BeforeState` (`bst`) and `AfterState` (`ast`). They describe the event's place in the DAG, which can differ from the room's current state after merging its branches.

| Stored event | Before context | After context |
| --- | --- | --- |
| Accepted non-state event | B | B |
| Accepted state event | B | B with its tuple replaced |
| Rejected event evaluated with known state | B | B |
| Soft-failed event | B | B with its tuple replaced, if it is a state event |
| Outlier or event staged without state | Unknown | Unknown |

The indexes have different purposes:

- `etv` records storage of a non-outlier event. Staged, rejected and soft-failed events have it too; it does not imply that an event has contexts or appears in a timeline.
- `evr` is the stream workers walk. Publication and outlier membership storage add rows; staging fetched events does not.
- `rmv` is the room timeline; `elv` is its locally sent subset used for federation. Rejected and soft-failed events enter neither.
- `rex` lists forward extremities. Accepted events replace the extremities they descend from.
- `rch` records each change to current state at the version of the event causing it. A batch can write several rows. A historical lookup takes the last row at or before its version; the room has empty state before its first row.

Versionstamps order publication and membership changes. They are distinct from context IDs: two versions can refer to the same state, and an event's after-context need not be the current context recorded at its publication version.

Event bodies are not content-addressed immutable records. They can be rewritten when an outlier is published or a remote join accepts an event previously stored as rejected.

### Room record and membership indexes

```text
id  / room_id                       -> Room (msgpack)
idd / room_id                       -> (depth)
iev / room_id                       -> latest event or receipt versionstamp
rlm / (room_id, user_id)             -> (event_id, room_id, "join")
pub / (-joined_member_count, room_id) -> empty

users/mem / (user_id, room_id)       -> (event_id, room_id, membership[, true])
users/mch / (user_id, versionstamp)  -> (event_id, room_id, membership)

servers/jcn / (room_id, server_name)     -> (joined_member_count)
servers/mem / (server_name, room_id)     -> (event_id, room_id, "join")
servers/mch / (server_name, versionstamp) -> (event_id, room_id, membership)
```

The room record stores:

| Field | Meaning |
| --- | --- |
| `CurrentState` (`cst`) | Current context ID |
| `StateRevision` (`srv`) | Revision checked by a send's publication guard; advances once per publication that stores new events, even if state is unchanged |
| `MemberCount` (`mem`) | Joined-member count |
| `LocalMembers` (`lmc`) | Joined members hosted here; greater than zero means this server is joined |
| `Encrypted` (`enc`) | Whether current state contains an encryption event |
| Summary fields | Name, topic, avatar, alias, join rule, history visibility and guest access from current state |
| Room metadata | Room version, type, federation setting and directory publication status |

Publication updates current state and the affected membership indexes in the same transaction. These indexes are not asynchronously rebuilt views.

**Local users.** Only local users have `users/mem` and `users/mch` rows. `rlm` lists the local users currently joined in each room. A membership change updates `mem`, sets or clears `rlm`, and writes `mch` when its event ID differs from the previous row. Removing a member tuple from state clears the rows without writing a membership-change row. An optional trailing `true` in `mem` marks an outlier invite, knock or other supported membership received while this server is outside the room.

**Servers.** `jcn` counts joined users of each server. Zero clears the count and current server membership. Crossing from zero to positive writes a join; crossing back writes a leave at the state step's version. When a publish brings this server into the room, other servers' initial joins get no `mch` row: federation starts their room stream from this server's join. This server's own join and other servers' leaves are still recorded.

**Room indexes.** `iev` lets sync skip unchanged rooms. `pub` orders published rooms by member count; storing a changed count moves the published-room key in the same transaction.

### Auth graph

An event's **auth chain** is the set of state events reached by following its `auth_events` references recursively: its direct auth events, their auth events, and so on. These are the events needed to check its authorization, including the authorization of those dependencies. For example, a message can reference the sender's joined membership and the room's power levels; the auth chain also includes the earlier events that authorized that membership and those power levels.

In room v12, authorization also depends on the create event identified implicitly by the room ID. `AuthDependencyIDs` includes it for fetching and event ordering, but the auth-chain index follows only the explicit `auth_events` references.

Despite the name, an auth chain can branch and share ancestors: it is a graph, not necessarily a linear sequence. `auth_events` links describe authorization dependencies; `prev_events` links describe predecessors in the room's event DAG. Being an earlier event in the room does not by itself make an event part of the auth chain.

The **chain cover** is an index over this graph. It groups events into numbered linear chains and records links between them, so reachability can be computed using chain positions instead of traversing every auth edge. These numbered chains are an indexing structure; one event's auth chain can span many of them.

```text
events/eah / event_id -> (1, chain, sequence, (auth_event_id, ...))
events/acp / (room_id, chain, sequence) -> (event_id)
events/acl / (room_id, chain, sequence, target_chain, target_sequence) -> empty
events/acn / room_id -> (last_allocated_chain_id)
```

Every stored event has an auth header. Its chain and sequence are zero until finalized; only state events get chain positions. Accepted state events are finalized during preparation or staging, and a stored outlier's header can be finalized later when another event needs it. Preparation holds new labels in memory until publication writes them. The leading `1` is the header format version. A finalized header supports at most 64 auth-event edges.

The cover is built as follows:

- An event extends an auth event's chain when that auth event has the same state tuple and is still the chain tip. Otherwise it starts a new chain.
- `(chain, sequence)` reaches every earlier position in its chain.
- Links record dependencies on other chains. Redundant links are omitted when another dependency or an earlier position already reaches the target.

A chain commonly follows one user's membership history or the room's power levels. Concurrent branches can create several chains for the same tuple. The tip is found through `acp`; `acn` allocates chain IDs.

Finalized positions and links are stable, but headers are rewritten on finalization and the allocator advances. Staging allocates and extends chains using normal transaction reads. Labels assigned during read-only preparation are checked again with normal reads in the publication transaction. [State resolution](event-sending.md#state-resolution) uses this cover to find auth differences and conflicted subgraphs without walking every auth edge individually.

## The immutable tree

Both maps use the same canonical tree, implemented in [state/page.go](../internal/databases/rooms/state/page.go) and [state/tree.go](../internal/databases/rooms/state/tree.go).

An entry's bucket is the first eight bytes of SHA-256 over its map key, interpreted as an unsigned big-endian integer. Each branch consumes one nibble of that bucket, giving 16 child slots. A path can be at most 16 branches deep.

```text
leaf   = (0, key, value, key, value, ...)
branch = (1, subtree_entry_count, subtree_entry_bytes,
          child_0, child_1, ..., child_15)
```

Leaf entries are sorted by key. Keys, values and child IDs are byte strings; an empty child ID means no subtree. Branch totals describe entries below the branch, not the size of the branch record itself.

A subtree becomes a leaf when it contains at most 64 entries and its encoded leaf fits within 64 KiB. Larger subtrees split by the next bucket nibble. Depth 16 always produces a leaf. A removal collapses a branch back to a leaf when the remaining contents fit; the stored totals let the tree make that decision before loading unchanged siblings.

Because the split and collapse rules depend on contents, the same map built through different sequences of updates has the same root.

### Updating a context

`Batch.TxnApply` reads the paths affected by a delta and creates replacement pages in memory. It leaves the old pages untouched. An empty event ID in a delta removes the tuple. Applying no effective change returns the original context ID.

For a member update, the non-member state root and unaffected member pages are shared:

```text
Before: C1 = (S1, M1)          After: C2 = (S1, M2)

M1 --+-- leaf A                M2 --+-- leaf A       shared
     +-- leaf B                    +-- leaf B       shared
     +-- leaf C                    +-- leaf C'      changed member

New records: leaf C', branch M2, context C2
Existing C1, M1, S1 and their pages remain usable.
```

A diff compares page IDs first and skips equal subtrees. Unequal subtrees are traversed to produce changed tuples, including the old and new membership values. Multi-context conflict discovery also retains absence: a tuple present in only some inputs conflicts.

### Reading, caching and writing

A `state.Batch` belongs to one room and holds the pages and contexts built during an operation. Reads follow this order:

```text
pending records in the Batch -> decoded process cache -> FDB snapshot reads
```

Missing pages are fetched together at each tree level. The process cache is a 64 MiB LRU keyed by room and hash. It contains committed pages and contexts only. Reads after state writes in the same transaction are excluded from the cache, because they may see uncommitted data. Records explicitly staged by a batch enter the cache after their staging transaction commits.

Snapshot reads are appropriate for immutable pages and contexts. Mutable room records, membership writes and auth-chain allocation have separate concurrency checks; they do not inherit that rule.

| Batch operation | Behaviour |
| --- | --- |
| `TxnLookupEntries` | Look up several tuples across both maps, batching reads by level |
| `TxnIterateState`, `TxnIterateMembers`, `TxnIterateAll` | Traverse selected maps or several contexts |
| `TxnMembersPage` | Resume a member scan by bucket, reading whole leaves until at least the requested limit; it can return more entries than the limit |
| `TxnCount` | Read the roots' entry totals |
| `TxnDiff`, `TxnConflicts` | Compare contexts while skipping shared pages |
| `TxnApply` | Build a new context from a delta, leaving its records pending |
| `TxnWrite` | Write requested pending contexts and the pending pages reachable from them into the caller's transaction |
| `Stage` | Write those records in separate bounded transactions before publication |

A member-scan continuation bucket belongs to the context it came from. Keep that context fixed for the whole scan.

Writing starts with children, then their parent pages, then context records. Unreachable intermediate results are omitted. Staging counts key and value bytes against `rooms.stateBudget.stagingBytes`, default 5,000,000 bytes; a record larger than the target is written alone. Staging does not move the room's current-state pointer. [Artifacts and staging](event-sending.md#staging-and-artifacts) explains how sends make staged data visible.

## Reading room state

Readers choose the context appropriate to the question:

| Question | Starting point |
| --- | --- |
| State now | `Room.CurrentState` |
| State before or after a particular event | Its `BeforeState` or `AfterState` |
| Current state at a sync version | Last `events/rch` row at or before that version |
| One member's membership in a room | Member lookup in the selected context |
| Rooms a local user belongs to | Their exact `users/mem` rows |
| Rooms a remote user belongs to | Rooms shared with their server, followed by a member lookup in each room's current context |
| Joined local users or servers in a room | `rlm` or `servers/jcn` |

Outlier local membership rows are the exception to current-state membership lookup while this server is outside the room. They preserve invites and knocks without inventing a full room context.

Sync derives state sections from context differences and uses the timeline and membership histories to choose their endpoints. It does not reconstruct state by replaying every state event. Directory search similarly checks remote candidates against current state in the requester's rooms, because remote users have no per-user membership rows.

## Storage constraints

The hash function, encodings, membership codes and tree-shape limits are part of the storage format. Changing them can change context IDs. A single entry must fit the leaf byte limit, fixed at 64 KiB in production; otherwise applying it returns `ErrEntryTooLarge`.

Pages, contexts, history rows and stored resolution results currently have no garbage collection. A send can leave staged data behind without publishing it. That data does not change current room state, but still consumes storage. Hashes are truncated to 128 bits; the format assumes distinct records do not collide.

Immutable state permits reads across multiple transactions for artifact jobs. Reads of mutable room state still need a consistent transaction and the publication checks described in [event sending](event-sending.md#publication-and-concurrency).

## Code map

| Code | Responsibility |
| --- | --- |
| [state/context.go](../internal/databases/rooms/state/context.go) | Context encoding, map operations and pending records |
| [state/page.go](../internal/databases/rooms/state/page.go), [state/tree.go](../internal/databases/rooms/state/tree.go) | Page format, hashing, traversal and updates |
| [state/state.go](../internal/databases/rooms/state/state.go), [state/stage.go](../internal/databases/rooms/state/stage.go) | FDB storage, cache eligibility and staged writes |
| [events/authgraph.go](../internal/databases/rooms/events/authgraph.go), [events/authchains.go](../internal/databases/rooms/events/authchains.go) | Auth headers and chain cover |
| [events/statehistory.go](../internal/databases/rooms/events/statehistory.go) | Current-state history by version |
| [eventsend.go](../internal/databases/rooms/eventsend.go) | Atomic publication of room fields, history and membership indexes |
