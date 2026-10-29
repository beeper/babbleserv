# Event sending

A send computes the state its events would produce, then publishes the events and the room's new state together. Small local sends do both in one FoundationDB transaction. Federated sends, remote joins and larger local sends prepare first and publish through a guarded write transaction. Work that is too large for those transactions is staged or computed between attempts.

[Room state storage](state-storage.md) describes the contexts, tree and FDB records used here. This document follows the transaction flow and explains which objects survive a retry.

## Entry points and ownership

Routes and [ingest jobs](#ingest-jobs) perform federation requests and signature checks before calling the databases. The rooms database receives local event content, verified federated events, and any fetched events or state supplied by another server. It does not perform network requests during a transaction.

| Entry point or type | Responsibility |
| --- | --- |
| `SendLocalEvents` / `localEventSender` | Build and authorize local events from current state; try a single transaction first |
| `SendFederatedEvents` / `federatedEventSender` | Check received events against their auth events, their before-state and current state |
| `RemoteJoin` / `remoteJoinSender` | Handle one local user's join with a supplied before-state while this server is outside the room, or in a state DAG room the state after its prev state events, in the phases of the room's ingest job |
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

Remote joins run in the room's [ingest job](#ingest-jobs), whose FoundationDB lock keeps them, and the work they stage, to one runner across processes. Other senders do not take that lock.

Notifications and retry pauses happen after releasing the room mutex. Requested event staging and artifact jobs also run outside it. Staging the pending contexts of an already prepared publication plan happens inside the attempt, before opening its publication transaction.

### Local sends

`localEventSender.sendInOneTransaction` holds the room mutex through a write transaction that prepares and publishes:

1. Check the client transaction ID, returning its existing event when present.
2. Read or create the room. An existing room must have local joined members.
3. Build the events in a line from current state. Set their prev/auth references, depth, timestamp and signatures, authorize them, and apply accepted state changes to the pending state batch.
4. Publish using the same transaction. Aliases, room publication, the client transaction ID and any requested lock refresh are included.

`RequireAllEvents` aborts the whole send if any event is rejected. Otherwise accepted events proceed and rejected local events are returned to the caller without being stored. A babble profile update requires its sender to be joined. A user's second `m.reaction` with the same key to the same event is refused, from the index of reactions the publish keeps; as in Synapse, only local sends are checked, and a federated event is never refused for it, which in a state DAG room would cut this server off from every event descending from it. The publish of an accepted redaction of a reaction clears its index row, as Synapse deletes the redacted event's relation, so the user can react with the key again.

In a state DAG room ([MSC4242](https://github.com/matrix-org/matrix-spec-proposals/pull/4242)) each event also cites the room's state extremities, read from `events/srx`, in `prev_state_events`. Its auth events are selected from its before-state as in other versions, but serialization drops them. Events built in a line advance the pending state extremities: an accepted state event replaces the ones it cites, so the next event of the batch cites it alone, while messages and rejected events leave them as they are. Before it is authorized, each event's `prev_state_events` are checked against the MSC's section 2 rules (state events of the room, not rejected), which hold by construction.

While there are at most 20 state extremities, every event cites them all and its before-state is the running current state, so no resolution is needed. Above 20, an event cites the 20 published last; depth would let senders choose. Its before-state is then the resolution of their after-contexts, and it must also pass current state, the resolution of every pending state extremity: other servers soft-fail a message that current state refuses, such as one from a user banned on a branch it does not cite. Resolutions go through `eventsendutil.Resolver`. One over the inline budgets returns an artifact job, after which the send prepares again. `prev_events` are capped at the 20 room extremities stored last in these rooms too. Both caps apply to received events as well: one citing more than 20 `prev_events` or `prev_state_events` is dropped (`types.CheckStateDAGFormat`), as Synapse drops it. The MSC states the `prev_state_events` cap under security considerations only, but upstream Synapse caps both ways, sending the 20 lowest event IDs and refusing more, so a room that took such an event would diverge from it.

An out-of-band rescind also cites the membership it ends (MSC4242): a leave sent by someone else for a user of another server whose server has no joined members here (`servers/jcn`), whose membership in current state is an invite or a knock, cites that membership beside the state extremities (`txnSupersededMembership`). The target's server holds the invite or knock without the state DAG, and the citation is the only way it can tell the leave supersedes it. At the cap the extremity stored first makes room for it. The event then cites other than every state extremity, so its before-state is the resolution of the cited after-contexts, as every other server works it out, and it must pass current state too. That resolution orders the old membership against later state by more than ancestry, so it can come out as older state, which the publish would make current everywhere: when the before-state differs from current state, the leave is sent citing the state extremities alone, with a warning, and the target's server may refuse it. The client route sends a rescind to the target's server as an outlier to-device row, which the federation sender sends as stored, as re-marshalling it without its room version would write the `auth_events` the target refuses. Other changes of a remote user's membership, and `make_*` templates, the target's own events, cite the state extremities alone.

`PrepareLocalEvents` runs the same preparation without publishing, for `make_join` and `make_knock` templates and remote invites, stamping the events once for every round. It resolves current state only when an event needs it, runs any resolution job it requests, then prepares again.

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

When this server is outside the room, ordinary federation sends accept only local-user invites and the inviter's leave rescinding one through the outlier-membership path. The write storing such a leave checks the membership it ends, and from `/send` that the room has no ingest job (`txnCheckNoIngestJob`), with normal reads, so a membership changed or a job started meanwhile conflicts with it; a snapshot read of `ijb` ahead of it only routes the PDUs to a job already running. Other events are dropped, and `make_join`, `make_knock` and `make_leave` answer 404. Client-initiated remote knocks and leaves call `SendFederatedOutlierMembershipEvent` directly after federation succeeds. If a local user rejects an invite and `make_leave` or `send_leave` fails, the client route instead signs and stores a local outlier leave, so the user can dismiss the invite even when the inviting server is unavailable or has left the room. A local user rescinding a knock stores one the same way. Unlike Synapse, which accepts such a leave into its stale room graph and forwards it, a server that has left never takes a remote invitee's rejection.

In a state DAG room a leave from outside must cite the invite it ends in `prev_state_events`, as the MSC has an out-of-band change do, since this server has no state DAG to place it in: a rescind not citing the invite is dropped. Another user's leave rejecting a local user's knock is never taken, as Synapse takes none: who may reject a knock is beyond what this server can check outside the room. A local outlier leave cites the invite or knock it ends.

#### State DAG rooms

In a state DAG room ([MSC4242](https://github.com/matrix-org/matrix-spec-proposals/pull/4242)) events cite no auth events, and the state before an event is the state after its `prev_state_events`. Preparation differs in four ways:

- **Order.** The batch is ordered by `prev_events` and `prev_state_events` (`AuthDependencyIDs`, which adds a v12 room's implicit create event).
- **Step 4** is skipped. Auth events an event was prepared with locally, as an invite of a remote user is, are discarded.
- **Step 5.** The MSC's section 2 comes first: an event citing a rejected event, a non-state event or another room's event is rejected, so rejection cascades along the state DAG, batch events carrying the outcome the attempt gave them. Such an event takes known empty state as its contexts, without the state before it being worked out: nothing reads it, as events citing it are rejected in turn. An event citing one not stored is dropped. Otherwise the before-state is the state after the `prev_state_events`, resolved when there are several. Each must have state here or be an earlier batch event; an event citing one stored without state is dropped with `ErrPrevStateEventsUnavailable`. `/send` passes only events whose prev state events are in place ([Routes](#routes)), so there are no boundaries, and a send with given states fails. Unknown `prev_events` change nothing but the room extremities and depth. The event's auth events are calculated from its before-state, as a local event's are, its header is finalized as above, and it is authorized. A duplicate takes the auth events stored for it, which resolutions read.
- **Step 6** holds only messages to current state, which soft fail as above. State events always join the state DAG. Each accepted state event advances the pending state extremities, and current state is the state after them, resolved when there are several; messages leave it alone. Room extremities advance for every accepted event, as in other versions.

Events stored already differ too. One stored without state, as a join or gap fill stages an event it then could not replay, is evaluated as new, its staged header replaced by its calculated auth events, as long as nothing gives it state before the publish, which reads it again. One a room's ingest job replayed but did not publish yet (`Unpublished`) is not a duplicate either: it keeps the state and auth header its replay gave it, skips step 5, and joins the state extremities in step 6, a run of them moving current state once, at its last event, ahead of the next event, which step 6 holds to it. Its publish rewrites its body without the mark and gives it `etv`, `evr`, `srx` and `sdg` rows but no timeline, extremity, relation or notification rows, as Synapse keeps the state events it fetches out of the timeline. An event citing such an event outside the batch is dropped (`citedUnpublished`), an unpublished one included: a published event never cites an unpublished one. That covers `send_join`, `send_knock` and `send_leave`, which take no fast-path check.

There is no readoption in these rooms: a rejection is permanent. A local user's remote join replays the room's state DAG first ([State DAG joins](#state-dag-joins)), then publishes in join mode as below, without a supplied before-state.

A prev event unknown here may arrive later, once an event citing it published. It then becomes a room extremity beside the events citing it, as the publish clears only the extremities it cites. That is harmless: the next local event cites it too, `prev_events` need not be tips, and replaces both, while current state in these rooms follows the state extremities, not the room extremities.

### Remote joins

A local user's join to a room this server is not in is a `RemoteJoin`, run by the room's ingest job. The resident server's `send_join` response supplies the state before it and its auth chain. The job runs the join's attempts in phases: `Stage` until the preparation asks for anything but staging, `BuildState` until it asks for no more preparatory work, and `Publish` through. An attempt stops with `errPhaseReached` once `eventSender.stopAt` accepts its preparation's outcome, before acting on it. Each phase prepares from scratch, so `Publish` redoes any work a change to the room invalidated since.

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
2. Evaluates notifications and stores event bodies, versions and worker rows. Accepted events also update timeline, relation, notification, depth and extremity records, and in a state DAG room accepted state events update the state extremities; the room's latest-version key advances.
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

Before comparing contexts without a stored result, preparation checks how many there are. After finding conflicts, it checks input tuple counts before enumerating their states. It requests a resolution artifact when any budget is exceeded:

| Setting | Default | Meaning |
| --- | --- | --- |
| `rooms.stateBudget.inlineResolutionContexts` | 64 | Distinct input contexts, which a state DAG event citing many prev state events can make large |
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

When the caller has no deadline, ordinary federated sends and local sends using the attempt loop get 60 seconds. Remote joins have no deadline of their own: their job stops once it makes no progress for its lease timeout, see [Lock and progress](#lock-and-progress), and `rooms.ingest.joinTimeout` bounds their `send_join`. Existing deadlines are preserved. The local single-transaction path uses the caller's context and configured FDB transaction limits.

| Bound | Behaviour |
| --- | --- |
| `rooms.stateBudget.stagingBytes`: 5,000,000 bytes by default | Target for state and event staging transactions |
| `config.PublishMandatoryMaxBytes`: 9,000,000 bytes | Publication fails with `ErrRoomTooLarge` when its approximate size exceeds this before the room record and extra writes |
| Artifact transaction age: 3 seconds | Renew before the next read batch |
| Inline resolution budgets | Return a work request instead of reading larger inputs inline |

Staging does not split the mandatory publication writes. A state reset with many local-member changes or server rows must still fit one publication transaction. An inline transaction timeout also does not automatically become an artifact job: jobs are requested through the explicit size and candidate budgets.

## Ingest jobs

A room's ingest job publishes events whose state is not stored here yet: local users' joins to rooms this server is not in, PDUs citing events this server does not store, and every PDU arriving for the room while it has a job. Each event it publishes is a target. The job fetches what its targets need from other servers, verifies and stages it, builds its state, then publishes the targets.

### Records

In the rooms database:

```text
ijb / room_id              -> IngestJob (msgpack)
ijt / (room_id, event_id)  -> IngestTarget (msgpack)
iju / (room_id, user_id)   -> IngestJoin (msgpack)
ijq / (room_id, counter)   -> int64, little-endian
bot / user_id              -> room_id
```

| Record | Holds |
| --- | --- |
| `ijb` | The room version. Its presence means the room has a job; nothing rewrites it, so it conflicts only with the job's start and end |
| `ijt` | Kind (`join` or `pdu`), the signed and verified event, the PDU's origin or the join's servers (the one whose `make_join` template it is first), enqueued time, whether a `send_join` may have landed and when the first was sent, failed rounds and when the last failed, a PDU's event bytes, how many fresh joins replaced the user's first, whether a PDU was pulled for another's prev events rather than sent here |
| `iju` | The user's join target, or why it failed, as the user's `/join` answers it |
| `ijq` | The PDU targets queued (`pdus`) and their event bytes (`bytes`), kept with one atomic add per counter by each transaction writing or clearing them, so they conflict with nothing. `FinishIngestJob` clears them with the job |
| `bot` | BabbleBot's direct message room with the user |

Rules every transaction keeps, so that two runners overlapping cost only duplicate work:

1. Writing a target writes the job if absent, reading `ijb` normally. The job is deleted only by `FinishIngestJob`, which reads the room's target range, so a target written meanwhile conflicts with the delete.
2. A failure writes only while its target exists (`FailIngestTarget`), so a runner that lost the job cannot fail a join another published.
3. Replacing a join target after a fresh `make_join` (`ReplaceIngestJoin`) checks `iju` still names the old event.
4. A publish clears the targets it is done with in its own transaction: stored PDUs whatever their outcome, joins once accepted, with their `iju`. A join stored rejected or soft failed keeps its target until the job fails it or joins the user with a new local event, whose publish takes the target and its `iju` in its transaction (`txnTakeIngestJoinTarget`, `SendLocalEventsOptions.IngestJoinTarget`). A join's publish, or that of the local join standing in for it, fails with `ErrIngestTargetGone` once its target is gone, failed or replaced by another runner, so the user is never joined while `iju` holds a failure.

Enqueueing skips PDUs stored already and those that are targets already, each event once however often a transaction carries it; in a state DAG room only an event stored with state and published is skipped, as the federated pipeline takes one replayed but unpublished, or staged without state, as new. A failed PDU target is removed and logged. A failed join's failure stays in `iju` until the user's next `/join` overwrites it.

### Lock and progress

The job's lock is the system database lease `room-ingest/<room_id>` (`util/lock`). `Jobs.Run` takes it with `WithLockIfAvailable`, so a room's job runs in one process; within a process, a second `Run` asks the running one to go through the targets again. The lease is refreshed on progress only, at most four times a lease (`rooms.ingest.leaseTimeout`, 2 minutes by default): a staging transaction, a renewed artifact read transaction, every 100 events verified, a signing key fetch, a `send_join` attempt, a read of a v1–v12 `send_join` response, a staged state DAG chunk holding events of the room, a server asked for state, a gap fetch or a `make_join`, a phase. Gap fill requests time out at half a lease, so the progress reported after one refreshes the lease in time. Waits bounded otherwise report progress every quarter lease while they last: a `send_join`'s response headers and a `/state` at a join. The key store fetches each server's keys once at a time, holding up only the callers that need them, so a response naming dead servers stalls neither other jobs nor `/send`. Work reports it with `util.ReportProgress`, which the job puts in its context. A watchdog ends the job's context a tenth of a lease before the lease written by the last refresh expires, and a refresh failing on the fencing token ends it at once.

A process runs at most `rooms.ingest.maxConcurrentJoins` (4) joins and `rooms.ingest.maxConcurrentGapFills` (16) gap fills at once across rooms' jobs, as each holds what it fetched. A unit finding no turn free does not wait, which would hold up the room's other work: it is held back without a failure counted, as are the round's later units of its kind, so they keep their order.

The `IngestJobWorker` scans `ijb` at startup and every minute in each process running workers, and runs each job. A job whose runner died resumes once its lease expires. After a job finishes, its runner reads `ijb` again: a target written after the delete, whose writer failed to take the lock the runner held, starts it again.

### Rounds and phases

A runner loops until the room has no targets. Each round reads the targets (collect), over several transactions at the read version of the first, so one `/send`'s PDUs are seen all together or not at all, retrying other retryable errors at that version and starting again, up to three reads, should the reads outlast it. Past that each page is read at its own version, and the targets citing a target that exists but was not read are left out of the round, as are those citing them. The room version's strategy groups them into units of work, each run through phase A (fetch, verify, stage), phase B (build state) and phase C (publish). A round ends after the first join it runs, so that the next collects the targets written meanwhile and puts their PDUs first. A unit failing holds back its targets, as does a unit whose targets failed less than 10 seconds before, that cites a target held back or that finds no turn free ([Lock and progress](#lock-and-progress)); the round's other units go on. A round holding back work ends the run, which runs again after 10 seconds, or 2 should a unit have found no turn free, unless another runner took its lock, or at once should this process be asked to run the job meanwhile, as a target written here asks, holding the same work back. So a target's failures count at most once a pause, however often `/send` enqueues into the room. A target whose work fails three times fails; a failure counts unless the job ended under it (the lease lost, the process stopping), or the watchdog ended a phase that had reported progress. Failures of this server's own, as a timeout, answer `/join` with a generic message, and BabbleBot's notices give a reason by error code only.

Room versions 1 to 12 use `legacyStrategy`, state DAG rooms `stateDAGStrategy` ([State DAG joins](#state-dag-joins)). Joins go oldest first. While this server is not in the room, the PDUs wait for a round without joins, so a join replaced by a fresh one or found done by another runner does not leave them to be dropped. The PDUs are ordered topologically, then split as `/send` splits them ([Routes](#routes)) into those connected to stored events and those with gaps, each in chunks of at most 50 events and 4 MB (the bytes counted when they were enqueued). In a room this server is in, a member's join is a whole `send_join`, which for a large room takes minutes, so the PDUs go first: the connected ones, then those with gaps, then the joins, the round ending after the first it runs, then the PDUs citing a pending join or a PDU citing one, as their gap fill would pull the join from another server: they run once the join published, or in a round holding it back. Connected PDUs cite none with a gap, so a gap fill failing holds back only the PDUs it is for and those citing them.

| Work | Phase A | Phase B | Phase C |
| --- | --- | --- | --- |
| Join | `send_join` with the stored event, then verify the response and `RemoteJoin.Stage` | `RemoteJoin.BuildState` | `RemoteJoin.Publish`; a join sent as a member's that is dropped or soft failed joins with a new local event |
| Connected PDUs | None | None | `SendFederatedEvents` |
| PDUs with a gap | In a joined room, the gap fill | None | `SendFederatedEvents` with the pulled events and given states, dropping those still citing missing events, but for those a server failed to answer for |
| State DAG PDUs with a gap | In a joined room, the prev events missing, then the state DAG gap fill ([State DAG gap fills](#state-dag-gap-fills)) | Replay of what it staged | The replayed events and the PDUs, in topological chunks, but for those a server failed to answer for |

A gap fill is the one `/send` used to run inline, moved into the job and run for a chunk: `get_missing_events` from the room's extremities to the PDUs citing missing prev events, then `/state_ids` at the prev events of pulled events that have no state here, from the server that pulled them, for at most 10 prev events. State is never fetched for the PDUs' own prev events, as Synapse refuses a pushed PDU whose prevs its sender cannot provide. A PDU's gap is open while it cites a prev event neither stored, in the chunk nor pulled. Each PDU's origin is asked for the open gaps of the PDUs it sent, every origin of the chunk, then the first five of the origins and the room's other servers for those still open. A response is read up to 62.5 MB and the limit asked. A request names at most two PDUs, and the events pulled so far among its earliest events: Synapse answers with at most 20 events, ignoring latest events it does not know, and pulls 10 itself for a PDU whose gap it fills. A server that failed or refused is not asked again within the fill. Each request times out at half a lease (60 seconds by default). A server is sent at most 10 requests a fill, and no request starts once the fill has run `rooms.ingest.gapFillTimeout` (5 minutes), those for the state at prev events included.

A PDU whose gap stays open publishes, to be dropped as `/send` dropped it, once every server asked for it answered or refused: a status below 500 other than 429, or a 2xx response that is not the JSON asked for or is over the size limit. A response body timing out or cut off is not a refusal. Should a server have failed the gap otherwise, as a timeout or a 5xx, or not been asked for it at the fill's bounds, or the state at a prev event the PDU reaches through pulled events have gone unfetched, failed other than by a refusal, past the bound of 10 or the fill's time, the PDU and the chunk's PDUs citing it stay targets and the work fails, while the rest of the chunk publishes; after three failures they fail, dropped with a log line. Nothing the fill fetches is stored before the publish stages it, so a resumed job fetches it again.

The local join a dropped or soft failed member's join falls back to carries a transaction ID derived from the `send_join` event, so a job resumed after sending it sends no other, and its publish takes the join target (rule 4).

The join target is written before `send_join` is sent, and records before each send that it may land, and when the first was sent. `send_join` is one `v2` request, read as it arrives (`Fetcher.SendJoin`, `SendJoinStateDAG`): the response may take `rooms.ingest.sendJoinResponseTimeout` (5 minutes) to start, as the resident builds what it serves first, and then fails once its body does not go on for 30 seconds. `rooms.ingest.joinTimeout` (10 minutes) after the first send bounds the sends and the waits for a response to start; a response that started may stream for `rooms.ingest.joinStreamTimeout` (2 hours), as a large room's state DAG takes long to stage, the stall timeout, the watchdog and the byte caps bounding it besides. A join timing out fails with 500 "Timed out joining the room". A v1–v12 response is decoded as a stream into state and auth chain held whole, no value it reads taking over 1 MiB.

| `send_join` outcome | Job |
| --- | --- |
| A response | Phase A carries on. More state and auth chain events than `rooms.ingest.maxJoinEvents`, or a body over `rooms.ingest.maxJoinResponseBytes` (512 MiB), fail the join with a 413 `M_TOO_LARGE` as soon as the decoder passes either |
| A 4xx other than 429, no earlier send that may have landed | Refused: the join fails with the remote error |
| A 4xx other than 429 after one that may have landed | Fetch the state before the join with `/state` at the join event from the join's servers, which may take as long as a `send_join` response to start; failing that, replace the target with a join from a fresh `make_join`, at most once |
| A 429, 5xx, timeout or broken connection | Send the same event again, across the join's servers, with pauses from 1 to 30 seconds, until `rooms.ingest.joinTimeout` |

A resident server answers a repeated `send_join` for a stored join as it did the first: Synapse recomputes the state before the event from its prev events, finds the event persisted already and answers with the same state, so a resumed join needs no second join event.

### State DAG joins

A state DAG room's job (`stateDAGStrategy`) orders its work as `legacyStrategy` does (`collectWorks`): while this server is not in the room the joins alone, oldest first, else the connected PDUs, those with gaps, the joins, then the PDUs citing a pending join, a round ending after its first join. A join goes through:

| Phase | Work |
| --- | --- |
| A | `send_join` with the stored event, read as a stream (`Fetcher.SendJoinStateDAG`), so a response of hundreds of megabytes is never held whole. Its `state_dag` arrives in chunks of at most 1,000 events and 4 MB, and no event, nor any other value the decoder reads, may take over 1 MiB: events stored already are skipped before their signatures are checked, the rest verified (format, signatures, hashes, redacted on a hash failure) and their state events staged (`StageStateDAGEvents`), with unfinalized headers citing nothing, as calculated auth events do not exist yet. The join itself is never staged, should a resident serve it, nor are the events served citing it, or one not staged so: they cannot have state before it publishes, so arrive as PDUs, and do not hide the tips they cite. Each chunk holding events of the room reports progress, those stored already included, so a server serving junk cannot keep the job going. More than `rooms.ingest.maxJoinEvents` events, or than that many times 64 KiB, fail the join with a 413 `M_TOO_LARGE`. Then the state DAG must connect the join to the create event (`MissingStateDAGEvents`): every event the join reaches along `prev_state_events`, through events without state here, must be stored |
| B | Replay (`ReplayStateDAG`): the events without state here that the join and the served state DAG's tips reach, in topological order, as step 5 gives a federated event its state, in staging transactions |
| C | The join publishes in join mode, the state before it being the state after its prev state events |

A join to a room this server got into meanwhile, after another join published, is a member's: its replay starts from its prev state events alone, the tips left to arrive as PDUs, and keeps what it replays unpublished, which phase C publishes ahead of the join, as a gap fill publishes ([State DAG gap fills](#state-dag-gap-fills)), before the join publishes as a member's.

A server answering with no state DAG, or one that does not connect the join, is not asked again, and the next is sent the join; once none is left the join fails (502). Such a server accepted the join, so another refusing it later neither fails nor replaces it: the join is sent again, with the usual pauses, until `rooms.ingest.joinTimeout`, then fails with the same 502. A join is replaced with a fresh one at most once. The request fails as a v1–v12 one does ([Rounds and phases](#rounds-and-phases)), the 30 seconds its body may stall not counting the time each chunk takes to verify and stage, and is then sent again as any `send_join` that may have landed. A resident refusing a join sent again after one may have landed, and no server having answered it, has the target replaced with a join from a fresh `make_join`, which may be of no other room version. BabbleBot names the room from the last name or canonical alias event of the state DAG staged so far.

Each replay transaction reads its events with normal reads and skips those with contexts, so a runner overlapping it conflicts with it. The events the job publishes rather than replays, the join, or a gap fill's PDUs, end its walk, and an event citing one stays without state. For each event: the MSC's section 2, then the state after its prev state events, resolved when there are several, then its calculated auth events, which replace the unfinalized header staged for it (`AuthGraph.AddCalculated`), authorization and the event's after-state, its header finalized and labelled as the publish would. The rewritten body, its contexts and pages, its header and chain rows are written together, and an accepted state event joins `events/sdg`, as the resident published it. Nothing touches the room record, its extremities or its timeline, so the replayed history is stored with state but never published; the join's reset of the state extremities covers it. Should the replay reach events a gap fill replayed but did not publish, it writes them as published history too, with their `sdg` rows. A transaction stops once it holds `rooms.stateBudget.stagingBytes`, those bodies counted too, has run 2 seconds or reached 1,000 events, or ahead of a resolution over the inline budgets, which runs as an artifact job before the next. The contexts are the progress: a job resumed sends `send_join` again, skipping what is stored, and replays what is still without state. An event reaching one missing here stays without, so a join citing one fails building its state, and a tip citing one stays out of the state extremities.

The join's publish takes as its before-state the state after its prev state events (`stateDAGJoinBefore`), built and diffed from the room's current state ahead of the publish as a join boundary is. It resets `rex` to the join and `srx` to the join and those of the served state DAG's tips, the events none of its events cites, that the join does not cite and that were accepted here, at most 1,000 of them, past which the join fails with a 413 `M_TOO_LARGE` before its replay: the join's template cites at most 20 of the resident's state extremities, and state may arrive there between `make_join` and `send_join`. Current state is the state after them, resolved when there are several, as artifact jobs do over the inline budgets. A served event rejected here hides the tips it cites. Further join targets then go through as members, and the PDUs queued meanwhile after them, publishing as in a joined room, with gap fills.

### State DAG gap fills

PDUs of a state DAG room this server is in that cite prev events not stored, or state events without state here or not published yet, are filled in chunks of the round's PDUs with gaps (`stateDAGPDUs`): phase A pulls the prev events, then the state DAG, phase B replays, phase C publishes.

1. **Prev events.** The PDUs sent here citing prev events neither stored nor among the PDUs have them pulled with `get_missing_events` without the state DAG flag, from the room's extremities, every origin asked for its own PDUs, two a request, as v1–v12 gap fills ask (`Fetcher.PrevEvents`). A response is read up to the limit asked; upstream Synapse refuses such a request in a state DAG room, which only leaves the prev events missing. The events pulled become PDU targets of the room's job too, marked pulled, written in transactions of at most 4 MB, and join the chunk. A pulled event's own prev events are never pulled, so the pulls stay bounded, and prev events still missing only leave their PDU a room extremity.
2. **State DAG.** `WalkStateDAGGap` walks back along `prev_state_events` from the PDUs, through events stored without state or replayed and not published, to published events; events reached that are not stored are missing. While a PDU reaches one, `get_missing_events` with `org.matrix.msc4242.state_dag` asks a server for them: `latest_events` the open PDUs, at most 100 a request, `earliest_events` the 100 state extremities published last, the limit 10, doubling each request up to 1,000, as the MSC recommends. A request fills the gap only when the walk after it finds an event missing before stored: a server sending other events, which it could send endlessly, gets no more requests for them. A request filling nothing, as a server capping its responses repeats the events nearest the PDUs, has the next request name the frontier instead, 100 of the events reached that cite missing ones, those named least in the fill first, so junk with low event IDs cannot crowd out the rest; once that fills nothing either, the next server is asked. A server is asked at most enough rounds to grow the limit to 1,000 and give a PDU its whole gap, one more and one from the frontier (`stateDAGRounds`, 19 for 10,000 events), and for at most 2 minutes, and a fill asks servers for at most `rooms.ingest.gapFillTimeout` (5 minutes), the prev events' pulls included. A server still giving the gap at those bounds, as one capping its answers at Synapse's 20 events would for a deep gap, leaves the PDUs whose gap is open held back, not failed. The PDUs' origins are asked first, every one of them, then the room's other servers while fewer than five are asked. A response over 62.5 MB is taken as a refusal; events served past the limit asked are dropped, the rest parsed, stored ones and the job's targets skipped before signatures are checked, and of the rest no more are verified and staged than the PDUs and the run may still fetch, and one more to show a gap past the bound, as a join stages them; then the walk runs again. The walk does not walk into the round's other targets not published or failed yet: a PDU reaching one, through events served, is held back with the PDUs citing it, as one a server failed to answer for is, while the other target publishes first; targets published or failed earlier in the round count as published or missing. A server failing is not asked again. A PDU still reaching a missing event once every server was asked fails, removed with a log line: no server could connect it to the create event, as the MSC requires of its sender. One a server failed to answer for, other than by refusing (a status below 500 other than 429, or a body that is not the JSON asked for), is held back with the PDUs citing it, its work failing for a later round while the rest publish. Three failures fail a target, so a PDU is held back for about three times `gapFillTimeout` at most. A PDU whose requests staged more than `rooms.ingest.maxGapEvents` (10,000) fails with a 413, and a run of the job whose gap fills staged more than `rooms.ingest.maxGapEventsPerRun` (100,000) fails the work, for the next run.
3. **Replay.** `ReplayStateDAG` from the PDUs' prev state events, the PDUs pending, keeping what it replays `Unpublished`. A replayed state event gets the content it replaces in its unsigned (`prev_content`), as the pipeline gives a published one, so sync serves it with that, joins' replayed history included.
4. **Publish.** The PDUs, with every event they reach that the job replayed but did not publish yet or that has no state, in topological order, in `SendFederatedEvents` chunks of at most 500 events and 4 MB, as the federated pipeline publishes replayed events above, without timeline rows and so without notifications, as Synapse keeps a gap's state out of its timeline. Each chunk reports progress. A chunk holding a PDU ends ahead of an event without state, which may cite it: that event is replayed as its chunk comes, once the PDUs it cites published.

The publish of a member's join to a state DAG room publishes what its replay reached the same way first.

### Routes

`/join` to a room this server is not in makes and signs the join as before, through the servers Synapse asks, in order: those of `via` and `server_name`, an alias's server and the servers its directory lookup gave, the room ID's domain, if it has one, and the server of the user's inviter, without repeats or this server (`remoteMembershipServers`, 404 `M_NOT_FOUND` with none, as for `/knock`), passing on a resident's 4xx refusal of `make_join` with its errcode, as Synapse does, and answering 502 for other failures, then `EnqueueIngestJoin` writes the job, the target and `iju`, and the handler runs the job and waits on `iju` with a FoundationDB watch, read again every 5 seconds should the watch fail. It answers once `iju` is cleared and the user is joined, or with the recorded failure. A user with a join pending attaches to it instead of making another, so a client retrying `/join` waits on the first join. Restricted joins are unsupported: a state DAG room's join template carrying `join_authorised_via_users_server` is refused with a 403 before anything is written, as the resident's co-signature in the `send_join` response is not taken, and the copy this server would publish without it is one other servers drop when they fetch the state DAG from here.

`/send` fetches nothing itself. A transaction of more than 50 PDUs or 100 EDUs is refused with a 400 `M_INVALID_PARAM`, as Synapse refuses it; the federation sender cuts its own at those limits. It verifies each PDU's format, signatures and hashes, then handles each room's PDUs on its own:

- **A room with a job:** every PDU is enqueued as a `pdu` target, the job is run, and the PDUs are answered 200.
- **A joined room without one:** in topological order, a PDU is connected when each prev and auth event it cites is stored here (staged events count) or is an earlier connected PDU of the transaction. In a state DAG room the prev state events, and the implicit create event, must be in place instead of the auth events: published here with state, or stored in another room, which the MSC's section 2 rejects citing, as an event ID fixes its room. A state event a gap fill replayed but did not publish yet is not in place, so that a published event never cites one. A PDU citing an unknown prev event goes to the job, which pulls it, and publishes the PDU even should it stay unknown, leaving it a room extremity, so a PDU citing one not arrived yet, as a sender in catch-up mode sends, is not dropped. The connected PDUs publish through one `SendFederatedEvents`, which reads `ijb` in its publish transaction: a job started meanwhile fails it with `ErrRoomIngesting`, and the route enqueues all the room's PDUs instead, as it does should the split's reads or the publish fail otherwise. The rest are enqueued after, for the job to fill their gaps, and answered 200. Enqueueing them first would start the job, which the publish would then defer to.
- **A room this server is not in, without a job:** outlier memberships as [above](#federated-sends), each write checking `ijb` with a normal read; should the send fail, as it does once a job started, the PDUs are enqueued.

An enqueue failing fails the transaction with a 500, which its sender sends again. Before enqueueing into a room that had a job, `/send` waits until the job's queue is below `rooms.ingest.maxQueuedPDUs` (2,000) PDUs and `rooms.ingest.maxQueuedPDUBytes` (50 MB), reading `ijq` every second outside any transaction. Other rooms' PDUs go on meanwhile. Should the queue stay full until four fifths of `rooms.ingest.queueWait` (50 seconds) after the request arrived, so that verifying the PDUs, waiting and the EDUs held for them stay within the minute Synapse waits, the transaction fails with a 429 `M_LIMIT_EXCEEDED`, which Synapse retries as it retries a timeout, sending the same transaction again. `/send` does not deduplicate transactions, so a retry is processed again: PDUs stored already are duplicates, and those already targets are not written again. EDUs are not idempotent (to-device messages would be delivered twice), so the EDUs of a transaction with PDUs for a room with a job are processed once its PDUs succeeded, and not at all when it fails. Other transactions' EDUs are processed alongside their PDUs, so that a room key sent to a device is not held behind the message it decrypts; such a transaction failing on an enqueue delivers its EDUs again with the retry. EDUs are processed until `queueWait` after the request arrived, so those held for the PDUs keep at least a fifth of it. A to-device message failing to store, or a device list or signing key update the deadline left unstored, fails the transaction with a 500: idempotent is not safe to drop, as the sender never sends it again. Presence is ephemeral, and a device list update failing otherwise would fail every resend, so those failures are only logged.

### BabbleBot

A local user the server owns (`babbleBot.localpart`, default `babblebot`), created at startup if missing and marked server-owned. Registration refuses the localpart; should a registered user have it already, BabbleBot is disabled rather than take that user over. Its direct message room with each user is created on first use, recorded in `bot` by the transaction creating it, so concurrent creators conflict, and added to the user's `m.direct`. Once a join target has been pending for `babbleBot.joinNoticeAfter` (60 seconds by default), BabbleBot tells the user the join is under way, naming the room from the `send_join` state once phase A has it, and follows up when the join publishes or fails. The notices' transaction IDs derive from the join event ID, so `eventTransactions` sends each once whatever process or runner sends it.

## Serving state DAG rooms

The federation endpoints other servers need in a state DAG room:

| Endpoint | Answer |
| --- | --- |
| `make_join`, `make_knock` | A template prepared as a local event, so citing the state extremities in `prev_state_events`, without `auth_events`. A `ver` list without the room's version is refused with `M_INCOMPATIBLE_ROOM_VERSION`, in every room version |
| `send_join` | The join goes through the federated pipeline, then the response is `{origin, event, state_dag}`, the state DAG being every accepted state event of the room published here before the join, as other servers published them (`events/sdg`, see [state-storage.md](state-storage.md#events-and-state-history)), its ancestors among them: not the join nor what was published after it, as upstream Synapse serves the join's ancestors only and would otherwise store the join without state first (`StreamServedStateDAG`, `ErrJoinNotInStateDAG` for a join without an `sdg` row). It is written as it is read, 500 events a transaction; a read failing part way, or a write taking over 30 seconds, leaves the body unfinished, which the joiner takes for a failed request. Each origin gets at most `federation.maxStateDAGJoinsPerOrigin` (3) such responses at once, as Synapse's `rc_federation` allows; its further `send_join`s wait for one to end, before their join is taken and holding no transaction, while their request lasts, and past `federation.maxQueuedStateDAGJoinsPerOrigin` (50) waiting are refused with 429 `M_LIMIT_EXCEEDED` (`originLimiter`). Once the join is verified, a room whose state DAG would hold more than `federation.maxServedStateDAG` (1,000,000) events with it is refused with 413 `M_TOO_LARGE` before the join is sent, rather than leave the joiner joined here alone; a join stored already counts in it, so is answered again as the first was |
| `make_leave`, `send_knock`, `send_leave` | As in other versions; a `make_leave` template is prepared as `make_join`'s, the target's own event citing the state extremities alone |
| `get_missing_events` | With `org.matrix.msc4242.state_dag`: the walk from `latest_events` along `prev_state_events`, a message's included, stopping at `earliest_events`, through events with state here that were not rejected. Events are ordered by their fewest hops from `latest_events`, then by event ID in byte order, and neither list is returned. More than 100 entries in either list is a 400. The response holds at most 1,000 events and stops before passing 8 MB, in that order. The walk reads 1,000 events a batch, the batches sharing a transaction until it is 3 seconds old or fails with a retryable error. Without the flag the walk follows `prev_events` only. Other room versions walk prev and auth events as before |
| `/event` | `{origin, origin_server_ts, pdus: [event]}` in every room version, peers fetching a message's `prev_state_events` through it. A rejected event is served, as Synapse serves it: the peer rejects what cites it itself |

## Code map

| Code | Responsibility |
| --- | --- |
| [eventsender.go](../internal/databases/rooms/eventsender.go) | Attempt driver, plans, guard, publication and resolution jobs |
| [eventsendlocal.go](../internal/databases/rooms/eventsendlocal.go) | Local event construction and single-transaction path |
| [eventsendfederated.go](../internal/databases/rooms/eventsendfederated.go) | Federated preparation, event staging and boundaries |
| [eventsendjoin.go](../internal/databases/rooms/eventsendjoin.go) | Remote-join checks, phases and mode changes |
| [statedag.go](../internal/databases/rooms/statedag.go) | State DAG extremities, step 6's rule, the state DAG `send_join` serves and the `get_missing_events` walk |
| [statedagjoin.go](../internal/databases/rooms/statedagjoin.go) | A state DAG join's staging, connectivity check, replay, state before the join and tips |
| [statedaggap.go](../internal/databases/rooms/statedaggap.go) | The walk of a state DAG gap fill |
| [ingest.go](../internal/databases/rooms/ingest.go) | Ingest job, target and join records |
| [ingest/](../internal/ingest) | Ingest job runner, strategies, the v1–v12 and state DAG join and PDU work, the connected split, gap fill fetches, state DAG gap fills, the streamed `send_join`, BabbleBot notices |
| [babblebot/](../internal/babblebot) | BabbleBot's user, rooms and notices |
| [eventsend.go](../internal/databases/rooms/eventsend.go) | Event and current-state publication writes |
| [eventsendutil/resolver.go](../internal/databases/rooms/eventsendutil/resolver.go), [reads.go](../internal/databases/rooms/eventsendutil/reads.go) | Context resolution and renewable transactions |
| [stateres/sparse.go](../internal/stateres/sparse.go), [v2.go](../internal/stateres/v2.go) | Sparse state resolution and iterative authorization |
