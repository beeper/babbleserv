package eventsendutil

import (
	"context"
	"errors"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/databases/rooms/events"
)

// An artifact job reads in a new transaction once its current one is this old (ahead of fdb's 5s limit)
const artifactJobTxnAge = 3 * time.Second

// Candidate events an artifact job resolving contexts reads per transaction
const artifactJobEventChunk = 1000

// resolutionReads are where resolving contexts reads: one transaction of a preparation, or the
// transactions of an artifact job. Reads of events and of the auth graph run in Do, which a job runs
// again in a new transaction when they fail with a retryable error. A job's state batch runs its
// own reads that way, whatever transaction it is given.
type resolutionReads interface {
	Do(reads func() error) error
	txn() fdb.ReadTransaction
	Events() *events.TxnEventsProvider
	graph() *events.AuthGraph
}

type preparationReads struct {
	readTxn        fdb.ReadTransaction
	eventsProvider *events.TxnEventsProvider
	authGraph      *events.AuthGraph
}

func (p preparationReads) Do(reads func() error) error       { return reads() }
func (p preparationReads) txn() fdb.ReadTransaction          { return p.readTxn }
func (p preparationReads) Events() *events.TxnEventsProvider { return p.eventsProvider }
func (p preparationReads) graph() *events.AuthGraph          { return p.authGraph }

// ArtifactReads gives an artifact job's reads a transaction younger than artifactJobTxnAge, moving
// the events read so far and the preparation's auth graph to a new one as the current one ages.
type ArtifactReads struct {
	ctx          context.Context
	db           fdb.Database
	eventsDir    *events.EventsDirectory
	tr           fdb.Transaction
	readTxn      fdb.ReadTransaction
	started      time.Time
	provider     *events.TxnEventsProvider
	authGraph    *events.AuthGraph
	transactions int
}

// NewArtifactReads carries successfully read events and finalized auth headers into renewable
// transactions. A nil graph starts a new job graph; a nil provider starts without carried events.
func NewArtifactReads(
	ctx context.Context,
	db fdb.Database,
	eventsDir *events.EventsDirectory,
	roomID id.RoomID,
	provider *events.TxnEventsProvider,
	graph *events.AuthGraph,
) (*ArtifactReads, error) {
	a := &ArtifactReads{ctx: ctx, db: db, eventsDir: eventsDir, authGraph: graph}
	if a.authGraph == nil {
		a.authGraph = eventsDir.NewJobAuthGraph(roomID, a.Do)
	}
	return a, a.start(provider)
}

// start begins a new transaction with the events carried read successfully.
func (a *ArtifactReads) start(carried *events.TxnEventsProvider) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	tr, err := a.db.CreateTransaction()
	if err != nil {
		return err
	}
	a.tr, a.readTxn, a.started = tr, tr.Snapshot(), time.Now()
	a.transactions++
	a.provider = a.eventsDir.NewTxnEventsProvider(a.ctx, a.readTxn)
	if carried != nil {
		a.provider.WithProviderEvents(carried)
	}
	a.authGraph.Renew(a.readTxn, a.Do)
	return nil
}

// Do runs a batch of reads in renewable transactions until complete or the context is canceled.
func (a *ArtifactReads) Do(reads func() error) error {
	if time.Since(a.started) >= artifactJobTxnAge {
		// Event reads started in the current transaction finish there, while it is young enough
		a.provider.Settle()
		if err := a.start(a.provider); err != nil {
			return err
		}
	}
	for {
		err := reads()
		var fdbErr fdb.Error
		if err == nil || !errors.As(err, &fdbErr) || a.tr.OnError(fdbErr).Get() != nil {
			return err
		}
		zerolog.Ctx(a.ctx).Warn().Err(err).Msg("Artifact job read failed, reading again in a new transaction")
		if err := a.start(a.provider); err != nil {
			return err
		}
	}
}

func (a *ArtifactReads) Read(batch func(fdb.ReadTransaction) error) error {
	return a.Do(func() error { return batch(a.readTxn) })
}

func (a *ArtifactReads) txn() fdb.ReadTransaction          { return a.readTxn }
func (a *ArtifactReads) Events() *events.TxnEventsProvider { return a.provider }
func (a *ArtifactReads) graph() *events.AuthGraph          { return a.authGraph }

func (a *ArtifactReads) Transactions() int { return a.transactions }

func (a *ArtifactReads) NewResolver(options ResolverOptions) *Resolver {
	resolver := newResolver(a, options)
	resolver.eventChunk = artifactJobEventChunk
	return resolver
}
