# Event sending

A send computes the state its events would produce, then publishes the events and the room's new state together. Small local sends do both in one FoundationDB transaction. Federated sends, remote joins and larger local sends prepare first and publish through a guarded write transaction. Work that is too large for those transactions is staged or computed between attempts.

[Room state storage](state-storage.md) describes the contexts, tree and FDB records used here. This document follows the transaction flow and explains which objects survive a retry.

## Entry points and ownership

Routes perform federation requests and signature checks before calling the databases. The rooms database receives local event content, verified federated events, and any fetched events or state supplied by another server. It does not perform network requests during a transaction.

| Entry point or type | Responsibility |
| --- | --- |
| `SendLocalEvents` / `localEventSender` | Build and authorize local events from current state; try a single transaction first |
| `SendFederatedEvents` / `federatedEventSender` | Check received events against their auth events, their before-state and current state |
| `remoteJoinSender` | Handle one local user's join with a supplied before-state while this server is outside the room |
| `eventSender` | Shared attempt loop, publication, artifact dispatch, retries and post-commit notification |
| `eventsendutil.Resolver` | Resolve contexts using either one preparation transaction or renewable artifact reads |

Local and federated senders embed `eventSender`. `remoteJoinSender` embeds `*federatedEventSender` and overrides preparation. If another join gets this server into the room first, it can use normal federated preparation.

The caller constructs one concrete sender for the whole send. Preparation produces fresh objects for each attempt:

| Object | Contents and lifetime |
| --- | --- |
| `preparedEvents` | Attempt-local event copies, pending `state.Batch`, auth graph, event provider, extremity replacements and current-state steps |
| `publishPlan` | Those prepared events, the room snapshot and guard, plus optional readoption, extremity reset and extra writes |
| `readyToPublish` | A plan for the current attempt |
| `needsWork` | Supporting data to build before preparing again |
| `alreadySent` | A stored result, such as a client transaction-ID duplicate or completed join |

`state.Batch` holds new state pages in memory until they are written. `AuthGraph` holds headers and chain labels assigned during preparation. `TxnEventsProvider` supplies both stored events and the attempt's pending events. A plan is valid only for the inputs recorded by its guard.

## Transaction flow

The common driver is `eventSender.run`, which calls `attempt` under the process's room mutex:

```text
lock room
  prepare (read transactions)
    |
    +-- readyToPublish
    |     stage oversized pending contexts if needed
    |     publish in one guarded write transaction
    |
    +-- needsWork ------------------------+
    |                                    |
    +-- alreadySent                      |
unlock room                              |
  |                                      |
  +-- success: notify and return          |
  +-- guard failed: pause, retry          |
  +-- build requested work <-------------+
        success: prepare again
        other error: return
```

`roomLocks` serialize attempts for the same room within this process. They do not exclude writers in another process. Cross-process correctness comes from the publication guard and FDB conflicts.

Remote joins also take a separate per-room `joinLocks` turn across their attempts and preparatory work. This prevents local remote-join workflows from duplicating expensive work. Other senders do not take that lock. A join waiting for a result already stored by another operation releases its join turn first.

Notifications and retry pauses happen after releasing the room mutex. Requested event staging and artifact jobs also run outside it. Staging the pending contexts of an already prepared publication plan happens inside the attempt, before opening its publication transaction.

### Local sends

`localEventSender.sendInOneTransaction` holds the room mutex through a write transaction that prepares and publishes:

1. Check the client transaction ID, returning its existing event when present.
2. Read or create the room. An existing room must have local joined members.
3. Build the events in a line from current state. Set their prev/auth references, depth, timestamp and signatures, authorize them, and apply accepted state changes to the pending state batch.
4. Publish using the same transaction. Aliases, room publication, the client transaction ID and any requested lock refresh are included.

`RequireAllEvents` aborts the whole send if any event is rejected. Otherwise accepted events proceed and rejected local events are returned to the caller without being stored. A babble profile update requires its sender to be joined.

If the pending contexts exceed the staging target, the callback returns `errNeedsStaging` without committing. The same sender switches to the common attempt loop: read-only preparation, state staging, then guarded publication. It keeps the original timestamp, but rebuilds events against the state seen by the new attempt.

### Federated sends

The batch is deduplicated and ordered so an event follows the batch events it cites as predecessors or auth events. Preparation copies the input events so one attempt's outcomes do not alter the originals.

Fetched events needed for supplied state are checked against their own auth events and staged first. Once available, a read transaction prepares the submitted batch:

1. **Auth events (step 4).** Check each new event against its declared auth events. Already stored events keep their stored outcome. Events depending on fetched events that could not be evaluated are dropped.
2. **Before-state (step 5).** Obtain the state after every predecessor, resolving their contexts when needed, or use an explicitly supplied before-state. Authorize the event against that context. An accepted state event replaces its tuple to produce its after-context; a non-state event keeps the same context.
3. **Current state (step 6).** Check accepted events against the running current state. Failure here soft-fails the event. Otherwise update the pending extremities and set current state to their sole after-context or the resolution of all their after-contexts.
4. Return a publication plan, or `needsWork` if a resolution exceeds an inline budget.

Step 5 and step 6 can resolve different sets of contexts. An event's before-state describes its predecessors. Current state merges the room's accepted forward extremities, including branches the event did not cite.

| Outcome | Stored? | Effect |
| --- | --- | --- |
| Accepted | Yes | Can advance current state, extremities and the timeline |
| Rejected | Yes, once its before-state is available | Before and after contexts are equal; excluded from current state and the timeline |
| Soft failed | Yes | Retains its DAG contexts, including its own state change; excluded from the current extremities and timeline at publication |
| Dropped | No new stored event | Can be evaluated again when missing dependencies become available |
| Duplicate | Existing event reused | Does not get published again |

Unknown predecessor state is a reason to drop an event, not to resolve only the known subset. When finalizing a state event's auth header in step 5, `ErrAuthEventPending` drops the event because its auth chain reaches an unsettled batch event; `ErrAuthEventNotFinalized` rejects it because an auth event cannot be finalized. Errors from resolving predecessor or extremity contexts generally fail the batch; an unsupported predecessor-resolution algorithm instead drops the affected event.

When this server is outside the room, ordinary federation sends accept only local-user invites and specific leaves responding to an existing invite through the outlier-membership path. Other events are dropped. Client-initiated remote knocks and leaves call `SendFederatedOutlierMembershipEvent` directly after federation succeeds. These paths do not construct or publish full room state.

### Remote joins

A single local user's join with `GivenStates.BeforeEvents` is treated as a remote join. The resident server's response supplies the state before it.

While this server is outside the room:

1. Check the room and response events. Authorize the join against its own auth events and the response state. A failed join aborts the send.
2. Stage the response events without contexts or worker/timeline rows.
3. Build a boundary context from the response state in an artifact job. Compute its diff from current state and stage the boundary.
4. Prepare and publish the join with before = boundary and after = boundary plus join. There is no current-state soft-fail check in this mode. Current state becomes the join's after-context and the join replaces the room's extremities.

The response may contain an event stored here as rejected that the join accepts. Staging finalizes its auth header; publication rewrites its stored outcome to accepted. This is readoption, and it is why event bodies cannot all be treated as immutable.

If another operation gets the server joined first, the sender switches to normal federated preparation. It reuses a boundary already built, where available, and now performs the current-state check. If the join is already stored without state, the sender waits while this server remains outside the room; once joined, it checks whether that event is the joiner's current membership.

### Supplied-state boundaries

A boundary describes state where the preceding history is unknown locally. `GivenStates` can supply state before a predecessor or before a submitted event, together with the event bodies needed to interpret it.

For a predecessor, its accepted state tuple is added to the supplied before-state to obtain an after-context. The ordinary federation boundary builder omits unknown, rejected, outlier, non-state and other-room events. Different events for the same state tuple, references to unsettled batch events, a missing create event or an entry too large for the tree make the boundary unusable. Building a predecessor boundary does not run step 5 on that predecessor.

A boundary can be built by changing current state or by building from empty; both produce the same context ID. The builder counts current state first and avoids enumerating it when it is more than twice the estimated size of the largest boundary being built. Otherwise it reads current state, then chooses whichever base needs fewer changes for each boundary. Ordinary federation boundaries are built during preparation. Remote-join boundaries use an artifact job.

## Publication and concurrency

`txnPublish` re-reads the mutable inputs with normal transaction reads before writing anything:

| Guard check | Why it is needed |
| --- | --- |
| Room `StateRevision` | A publication since preparation can invalidate the plan |
| Whether this server is joined | Distinguishes normal sends from remote joins |
| Event IDs expected to be unstored | Staging can store dependencies without changing the room revision |
| Auth-chain tips, allocator and headers used for labels | Staging can allocate or finalize labels without publishing room state |

A mismatch returns `errGuardFailed`, including `errEventsStoredConcurrently`. A concurrent write after these reads conflicts with the publication transaction. The room mutex alone cannot provide either check across processes.

After the guard, a publication with new events:

1. Rewrites any rejected response events accepted by the remote join.
2. Evaluates notifications and stores event bodies, versions and worker rows. Accepted events also update timeline, relation, notification, depth and extremity records; the room's latest-version key advances.
3. Writes unstaged contexts and pages, finalized auth headers and chain records.
4. Diffs each current-state step against the previous one and writes an `events/rch` row at the event's version. The diffs update room summary fields and counts, server memberships and local-user membership rows and histories.
5. Checks the transaction's approximate size. It then stores the room record with its incremented revision, moving the published-room index if the joined-member count changed.
6. Applies a join's extremity reset and any extra writes: alias, room publication, transaction ID or lock refresh.

These writes commit together. Staged pages can exist earlier, but current state and its membership indexes move only in this publication. A batch can contain several state steps, so its history preserves intermediate transitions even though the final room record holds only the last context.

With no new events to store, the state writes and revision increment are skipped; any extra writes can still run.

After a successful commit, `afterSend` sends the notifier change and logs the result. Client transaction-ID duplicates suppress that notification.

## Staging and artifacts

Staging writes supporting data ahead of publication. Artifact jobs build remote-join boundaries and run resolutions that exceed the inline preparation budgets. Both can use several transactions, but they leave the room's current-state pointer and publication revision alone.

| Work | Output | Where the next attempt finds it |
| --- | --- | --- |
| `stageEventsWork` | Checked fetched or join-response events; finalized auth headers for accepted state events | Event and auth indexes |
| `joinStateWork` | Staged before-context and its diff from the room snapshot | `remoteJoin.boundary` and `remoteJoin.diff` on the surviving sender |
| `resolveStateWork` | Staged resolved context, then a stored result lookup | `state/rs`, keyed by input contexts and implementation version |

Join and resolution work have stable artifact keys. Requesting the same key again after it was successfully built is an error, preventing an endless build loop. Event staging has no such key: concurrent storage can legitimately require another check of the remaining events.

### Event staging transactions

`stageStatelessEvents` writes checked events in dependency order, chunked by the staging target and versionstamp capacity. Each write transaction:

- Checks joined mode from a snapshot of the room record.
- Checks that newly staged events are still unstored and that auth events previously read as rejected have not been accepted meanwhile.
- Assigns auth-chain labels to accepted state events using normal reads of the allocator and chain tips. New rejected and non-state events get unfinalized headers.
- Writes event bodies, `etv` and auth records, without `evr` or timeline rows. A readoption candidate gets its header finalized here, with the body rewrite left to publication.

This path does not increment the room revision. A staged event already counts as stored for duplicate detection, even though its before/after contexts remain unknown.

### State staging transactions

A batch stages only the requested contexts and the pending pages they reach. Children are written before parents, and all pages precede the context records. Transactions target 5 MB of key and value bytes; the budget intentionally leaves headroom instead of estimating protocol overhead precisely.

A ready publication plan stages its contexts when their pending bytes exceed that target. A resolution job stages any pending input contexts before it starts and stages its result before writing `state/rs`. A join-state job computes its diff while the new pages are still available in its batch, then stages the boundary.

An abandoned send can therefore leave stored supporting data. Staging itself neither makes the room joined nor sends worker notifications.

### Reads across transactions

`eventsendutil.Resolver` uses `preparationReads` for an inline resolution. `ArtifactReads` runs the same resolution logic through renewable snapshot transactions. A `state.NewJobBatch` routes its page and context reads through the same renewal callback.

Before another read batch begins, a transaction at least 3 seconds old is replaced. Successfully read events and the auth graph are carried over. A retryable FDB read error retries that batch in a new transaction. Batches are bounded: 128 pages, 1,000 contexts, headers or candidate events, up to 10,000 link rows, and 5,000 chain positions.

The job is tied to specific input contexts, not to a moving room snapshot. Pages and contexts are immutable, and finalized auth positions are stable. The graph retains positions assigned during preparation and does not read later chain extensions into those positions. Jobs do not assign new labels. Stored event outcomes can change through readoption, so the stored resolution key should not be mistaken for a version of every event body it read.

After the job, preparation starts again against the room's current record. A join boundary diff is reusable only while its starting context still matches. A resolution result is reusable for the same input context set.

## State resolution

Resolution is needed both to find the state before an event with several predecessors and to find current state after several accepted extremities. Equal context IDs need no resolution. A single known predecessor supplies its after-context directly. With no predecessors, the before-state is known empty state.

`eventsendutil.Resolver` connects the stored contexts and auth graph to the algorithm in `internal/stateres`:

```text
input context IDs
  -> reuse a stored resolution result, if present
  -> compare the state/member trees to find conflicting tuples
  -> compute the auth-chain difference of the input states
  -> add the conflicted subgraph for state resolution v2.1
  -> read candidate events and authorize them in resolution order
  -> obtain a delta against the first input
  -> apply the delta to produce the result context
```

### Conflicts and auth difference

Tree comparison skips equal subtrees. A tuple whose event differs between inputs conflicts; absence in one input also counts. An input state's [auth chain](state-storage.md#auth-graph) is the union of the auth ancestry of its state events. Candidate events include those conflicting events and events reachable through some input states' auth chains but not all.

To compute the auth difference, the graph reads every input state event's finalized chain position. For each input, it follows chain links to find the highest reachable sequence on every chain, including the starting state events themselves. On a chain, positions above the minimum reach and at or below the maximum reach form the difference. A chain not reached by an input has reach zero.

```text
                 chain 1   chain 2
input A reaches      5         2
input B reaches      3         2

auth difference: chain 1 positions 4 and 5
```

This can include an auth ancestor that appears in neither input state. Exact common-state events are removed from the strict auth difference by the sparse resolver.

For v2.1, the graph also finds events that are both ancestors and descendants of conflicted events. It intersects backward and forward reach on each chain and reads the resulting intervals. Common events in this conflicted subgraph are retained as candidates.

### Resolving candidates

`ResolveSparse` receives the conflicts, auth difference, optional conflicted subgraph and a lookup into the first context. It returns only changes relative to that context.

Power events and their auth ancestors within the candidate set are processed in reverse topological power order, followed by other candidates in mainline order. Each stage runs iterative authorization. V2 can read common state during these checks; v2.1 begins from empty partial state. Common state is restored in the final result. The resolver supplies membership values for changed member tuples before applying the delta.

The implementation supports state resolution v2 and v2.1. Distinct contexts requiring an older algorithm fail with `ErrUnsupportedAlgorithm`. Rejected candidates are skipped during iterative authorization; accepting them would also require changing their stored rejection status and dependent state. Authorization is delegated through `util.Authorize` to `gomatrixserverlib.Allowed`, with lookup errors kept separate from authorization failures.

Sparse resolution avoids copying unchanged state into the result, but computing the auth difference still enumerates the input state events and their headers. Its cost is not limited to the visibly conflicting tuples.

### Inline budgets

After finding conflicts, preparation checks input tuple counts before enumerating their states. It requests a resolution artifact when either budget is exceeded:

| Setting | Default | Meaning |
| --- | --- | --- |
| `rooms.stateBudget.inlineResolutionStateTuples` | 10,000 | Largest input context's tuple count |
| `rooms.stateBudget.inlineResolutionCandidates` | 2,000 | Distinct candidate events |

The candidate budget is checked after the auth difference and again after adding the v2.1 subgraph. Jobs run without those inline limits and read candidates in chunks. The completed result is staged and stored for the next attempt.

## Retries and bounds

There are three retry levels:

| Level | What repeats |
| --- | --- |
| FDB transaction retry | The read or write callback, through the transaction helpers |
| Send attempt retry | Preparation and publication after a failed guard, or while a stored join remains unpublished |
| Artifact read retry | A failed bounded batch of reads, keeping successfully read data |

Successful preparatory work immediately starts a new attempt. Guard failures and `errJoinUnpublished` pause with jitter: the base delay starts at 10 ms and doubles to a 250 ms cap, with each actual pause between half and the full base delay. Other errors return to the caller.

When the caller has no deadline, ordinary federated sends and local sends using the attempt loop get 60 seconds; remote joins get 10 minutes. Existing deadlines are preserved. The local single-transaction path uses the caller's context and configured FDB transaction limits.

| Bound | Behaviour |
| --- | --- |
| `rooms.stateBudget.stagingBytes`: 5,000,000 bytes by default | Target for state and event staging transactions |
| `config.PublishMandatoryMaxBytes`: 9,000,000 bytes | Publication fails with `ErrRoomTooLarge` when its approximate size exceeds this before the room record and extra writes |
| Artifact transaction age: 3 seconds | Renew before the next read batch |
| Inline resolution budgets | Return a work request instead of reading larger inputs inline |

Staging does not split the mandatory publication writes. A state reset with many local-member changes or server rows must still fit one publication transaction. An inline transaction timeout also does not automatically become an artifact job: jobs are requested through the explicit size and candidate budgets.

## Code map

| Code | Responsibility |
| --- | --- |
| [eventsender.go](../internal/databases/rooms/eventsender.go) | Attempt driver, plans, guard, publication and resolution jobs |
| [eventsendlocal.go](../internal/databases/rooms/eventsendlocal.go) | Local event construction and single-transaction path |
| [eventsendfederated.go](../internal/databases/rooms/eventsendfederated.go) | Federated preparation, event staging and boundaries |
| [eventsendjoin.go](../internal/databases/rooms/eventsendjoin.go) | Remote-join checks and mode changes |
| [eventsend.go](../internal/databases/rooms/eventsend.go) | Event and current-state publication writes |
| [eventsendutil/resolver.go](../internal/databases/rooms/eventsendutil/resolver.go), [reads.go](../internal/databases/rooms/eventsendutil/reads.go) | Context resolution and renewable transactions |
| [stateres/sparse.go](../internal/stateres/sparse.go), [v2.go](../internal/stateres/v2.go) | Sparse state resolution and iterative authorization |
