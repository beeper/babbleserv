// Package ingest runs rooms' ingest jobs: per room, one runner at a time fetches what the room's
// targets need from other servers, stages it, builds its state, then publishes the targets. Joins and
// the PDUs arriving while a room has a job are targets. See docs/event-sending.md#ingest-jobs.

package ingest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/babblebot"
	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

var (
	// Ends a unit of work early, its targets published, failed or replaced
	errWorkDone = errors.New("the work's targets are done")
	// Ends a round that held back work failing or that failed within the retry pause
	errWorkHeld = errors.New("the job held back work that failed")
	// Causes ending a job's context
	errLeaseLost  = errors.New("the job's lock lease was lost")
	errNoProgress = errors.New("the job made no progress for its lease timeout")
)

// A target whose work fails this many times, other than by its job ending, fails
const maxTargetFailures = 3

// The pause before work that failed runs again, and a job that stopped on an error, sooner than the
// IngestJobWorker's scan
const jobRetryPause = 10 * time.Second

func lockName(roomID id.RoomID) string {
	return "room-ingest/" + roomID.String()
}

// Jobs runs rooms' ingest jobs. A room's job runs in at most one process, which holds its lock, and
// is resumed by the IngestJobWorker once its runner stops.
type Jobs struct {
	log      zerolog.Logger
	config   config.BabbleConfig
	db       *databases.Databases
	fclient  fclient.FederationClient
	keyStore *util.KeyStore
	fetcher  *Fetcher
	bot      *babblebot.Bot

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// Rooms whose job this process runs, and whether it was asked to run it again meanwhile
	running map[id.RoomID]*roomRunner
	// The turns of joins and gap fills, of which the process runs a bounded number at once
	turns map[workKind]chan struct{}
}

type roomRunner struct {
	again bool
}

func NewJobs(
	cfg config.BabbleConfig,
	logger zerolog.Logger,
	db *databases.Databases,
	fclient fclient.FederationClient,
	fetcher *Fetcher,
	keyStore *util.KeyStore,
	bot *babblebot.Bot,
) *Jobs {
	log := logger.With().Str("component", "ingest").Logger()
	ctx, cancel := context.WithCancel(log.WithContext(context.Background()))
	return &Jobs{
		log:      log,
		config:   cfg,
		db:       db,
		fclient:  fclient,
		keyStore: keyStore,
		fetcher:  fetcher,
		bot:      bot,
		ctx:      ctx,
		cancel:   cancel,
		running:  make(map[id.RoomID]*roomRunner),
		turns: map[workKind]chan struct{}{
			workJoin:    make(chan struct{}, cfg.Rooms.Ingest.MaxConcurrentJoins),
			workGapFill: make(chan struct{}, cfg.Rooms.Ingest.MaxConcurrentGapFills),
		},
	}
}

// turn takes a turn to run work of the kind, waiting for one while the process runs as many as it
// may. Waiting reports progress, so that the job keeps its lease.
func (j *Jobs) turn(ctx context.Context, kind workKind) (release func(), err error) {
	turns := j.turns[kind]
	if turns == nil {
		return func() {}, nil
	}
	select {
	case turns <- struct{}{}:
		return func() { <-turns }, nil
	default:
	}
	zerolog.Ctx(ctx).Debug().Msg("Waiting for a turn to run ingest work")
	defer reportProgressEvery(ctx, j.config)()
	select {
	case turns <- struct{}{}:
		return func() { <-turns }, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// reportProgressEvery keeps a job's lease through a wait bounded otherwise, refreshing it as often
// as progress does
func reportProgressEvery(ctx context.Context, cfg config.BabbleConfig) (stop func()) {
	return util.ReportProgressEvery(ctx, cfg.Rooms.Ingest.LeaseTimeout/4)
}

// Stop ends the jobs this process runs, releasing their locks for other processes to resume them
func (j *Jobs) Stop() {
	j.mu.Lock()
	j.cancel()
	j.mu.Unlock()
	j.wg.Wait()
}

// Run runs the room's job in the background, unless another runner holds its lock. When this process
// runs it already, the runner goes through the room's targets again before it stops.
func (j *Jobs) Run(roomID id.RoomID) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if runner, found := j.running[roomID]; found {
		runner.again = true
		return
	} else if j.ctx.Err() != nil {
		return
	}
	runner := &roomRunner{}
	j.running[roomID] = runner
	j.wg.Go(func() {
		log := j.log.With().Stringer("room_id", roomID).Logger()
		defer j.forget(roomID, runner)
		defer util.RecoverPanic(&log, nil)
		j.runRoom(log.WithContext(j.ctx), roomID, runner)
	})
}

// forget stops tracking a runner that stopped, as one that panicked does without deciding to
func (j *Jobs) forget(roomID id.RoomID, runner *roomRunner) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running[roomID] == runner {
		delete(j.running, roomID)
	}
}

// runRoom runs the room's job while this process can take its lock and work remains. Once the job
// finished, a target written since starts it again: its writer may have failed to take the lock this
// runner held, or asked this runner to run again. A job that held back failed work runs again at
// once when asked to, for the targets written since, else after a pause, as does a job stopped by an
// error, unless another runner took its lock.
func (j *Jobs) runRoom(ctx context.Context, roomID id.RoomID, runner *roomRunner) {
	for {
		finished, err := j.runOnce(ctx, roomID)
		held := errors.Is(err, errWorkHeld)
		if held {
			zerolog.Ctx(ctx).Info().Err(err).Msg("Ingest job held back failed work, it runs again after a pause")
		} else if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Ingest job stopped, it resumes later")
		}
		var restarted bool
		if finished && err == nil {
			job, err := j.db.Rooms.GetIngestJob(ctx, roomID)
			restarted = err == nil && job != nil
		}
		j.mu.Lock()
		again := finished && err == nil && (restarted || runner.again) || held && runner.again
		runner.again = false
		if !again || ctx.Err() != nil {
			delete(j.running, roomID)
			j.mu.Unlock()
			if err != nil && !errors.Is(err, errLeaseLost) && ctx.Err() == nil {
				time.AfterFunc(jobRetryPause, func() { j.Run(roomID) })
			}
			return
		}
		j.mu.Unlock()
	}
}

// runOnce runs the room's job if its lock is free, reporting whether it ran it until it had no
// targets left
func (j *Jobs) runOnce(ctx context.Context, roomID id.RoomID) (bool, error) {
	var finished bool
	var runErr error
	acquired, err := lock.WithLockIfAvailable(ctx, j.db.System, lockName(roomID), lock.LockOptions{
		Timeout: j.config.Rooms.Ingest.LeaseTimeout,
	}, func(roomLock lock.Lock) {
		defer roomLock.Release()
		finished, runErr = j.runJob(ctx, roomID, roomLock)
	})
	if err != nil {
		return false, fmt.Errorf("failed to take the job's lock: %w", err)
	} else if !acquired {
		zerolog.Ctx(ctx).Debug().Msg("Another runner holds the ingest job's lock")
	}
	return finished, runErr
}

// runJob runs rounds until the room has no targets, then deletes the job. Each round collects the
// targets and runs the work its strategy groups them into, each through its phases.
func (j *Jobs) runJob(ctx context.Context, roomID id.RoomID, roomLock lock.Lock) (bool, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	run := &jobRun{
		jobs:      j,
		roomID:    roomID,
		lock:      roomLock,
		cancel:    cancel,
		gapBudget: j.config.Rooms.Ingest.MaxGapEventsPerRun,
		refreshed: time.Now(),
		notices:   make(map[id.EventID]*time.Timer),
	}
	run.watchdog = time.AfterFunc(run.watchdogTimeout(), func() { cancel(errNoProgress) })
	defer run.stop()
	ctx = util.WithProgress(ctx, run.progress)
	log := zerolog.Ctx(ctx)
	log.Info().Msg("Running ingest job")

	for {
		job, targets, err := j.db.Rooms.GetIngestTargets(ctx, roomID)
		if err != nil {
			return false, run.ended(ctx, err)
		} else if job == nil {
			return true, nil
		} else if len(targets) == 0 {
			if finished, err := j.db.Rooms.FinishIngestJob(ctx, roomID); err != nil {
				return false, run.ended(ctx, err)
			} else if finished {
				log.Info().Msg("Ingest job finished")
				return true, nil
			}
			continue
		}
		run.roomVersion = job.RoomVersion
		if err := run.round(ctx, targets); err != nil {
			return false, run.ended(ctx, err)
		}
	}
}

// jobRun is one runner's run of a room's job while it holds the room's lock
type jobRun struct {
	jobs        *Jobs
	roomID      id.RoomID
	roomVersion string
	lock        lock.Lock
	cancel      context.CancelCauseFunc
	// The round's targets not published or failed yet, and the state DAG events the run's gap fills
	// may fetch still
	targets   map[id.EventID]struct{}
	gapBudget int

	mu        sync.Mutex
	refreshed time.Time
	watchdog  *time.Timer
	// Whether the current phase reported progress
	progressed bool
	roomName   string
	notices    map[id.EventID]*time.Timer
}

// progress refreshes the lock's lease, at most four times a lease. The watchdog stops the run shortly
// before the lease would expire without, and a lease lost to another runner stops it at once.
func (run *jobRun) progress() {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.progressed = true
	if time.Since(run.refreshed) < run.jobs.config.Rooms.Ingest.LeaseTimeout/4 {
		return
	} else if !run.refreshLock() {
		run.cancel(errLeaseLost)
		return
	}
	run.refreshed = time.Now()
	run.watchdog.Reset(run.watchdogTimeout())
}

// watchdogTimeout is the time from a refresh to just before the lease it writes expires
func (run *jobRun) watchdogTimeout() time.Duration {
	lease := run.jobs.config.Rooms.Ingest.LeaseTimeout
	return lease - lease/10
}

// startPhase resets whether the phase about to run reported progress
func (run *jobRun) startPhase() {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.progressed = false
}

// stalled reports whether the current phase ended without reporting any progress
func (run *jobRun) stalled() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return !run.progressed
}

// refreshLock refreshes the lease, which panics once another runner took the lock
func (run *jobRun) refreshLock() (refreshed bool) {
	defer func() {
		if r := recover(); r != nil {
			run.jobs.log.Warn().Stringer("room_id", run.roomID).Any("recover", r).Msg("Lost the ingest job's lock")
			refreshed = false
		}
	}()
	run.lock.Refresh()
	return true
}

func (run *jobRun) stop() {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.watchdog.Stop()
	for _, timer := range run.notices {
		timer.Stop()
	}
}

// ended is why the run ended: what ended its context, if anything did, else the error
func (run *jobRun) ended(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// round publishes the targets a round collected, through the work the room version's strategy groups
// them into. Work failing holds back its targets, and work whose targets failed within the retry
// pause or cite a target held back is held back too, until a later round after the pause: asking
// again at once, as each PDU enqueued meanwhile would, could fail the targets within seconds. The
// rest of the work goes on, but for the work after a join: a join can take minutes, so the round
// ends after one, and the next collects the targets written meanwhile, putting their PDUs first.
func (run *jobRun) round(ctx context.Context, targets []*types.IngestTarget) error {
	strategy, err := strategyFor(run.roomVersion)
	if err != nil {
		return run.failAll(ctx, targets, failureFromError(err))
	}
	run.scheduleJoinNotices(targets)
	run.targets = make(map[id.EventID]struct{}, len(targets))
	for _, target := range targets {
		run.targets[target.Event.ID] = struct{}{}
	}
	works, err := strategy.collect(ctx, run, targets)
	if err != nil {
		return err
	}
	held, errs, err := run.runWorks(ctx, works)
	if err != nil {
		return err
	} else if len(held) == 0 {
		return nil
	} else if len(errs) == 0 {
		return fmt.Errorf("%w: %d targets", errWorkHeld, len(held))
	}
	return fmt.Errorf("%w: %d targets: %w", errWorkHeld, len(held), errors.Join(errs...))
}

// runWorks runs a round's work in order, up to and including the first join run, returning the
// targets held back and why, or what ended the job's context
func (run *jobRun) runWorks(ctx context.Context, works []ingestWork) (map[id.EventID]struct{}, []error, error) {
	now := time.Now()
	held := make(map[id.EventID]struct{})
	var errs []error
	for _, work := range works {
		if heldBack(work.targets(), held, now) {
			hold(work.targets(), held)
			continue
		}
		err := run.runTurn(ctx, work)
		if cause := context.Cause(ctx); cause != nil {
			return nil, nil, cause
		} else if err != nil {
			errs = append(errs, err)
			hold(work.targets(), held)
		}
		if work.kind() == workJoin {
			break
		}
	}
	return held, errs, nil
}

// heldBack reports whether a target of work failed within the retry pause or cites one held back
func heldBack(targets []*types.IngestTarget, held map[id.EventID]struct{}, now time.Time) bool {
	return slices.ContainsFunc(targets, func(target *types.IngestTarget) bool {
		return now.Sub(time.UnixMilli(target.FailedAt)) < jobRetryPause || citesAny(target.Event, held)
	})
}

func hold(targets []*types.IngestTarget, held map[id.EventID]struct{}) {
	for _, target := range targets {
		held[target.Event.ID] = struct{}{}
	}
}

// untarget stops counting targets as the round's once published or failed, so that other targets'
// gap fills reach them as published, or as missing
func (run *jobRun) untarget(eventIDs ...id.EventID) {
	for _, eventID := range eventIDs {
		delete(run.targets, eventID)
	}
}

// runTurn runs a unit of work once the process has a turn for its kind
func (run *jobRun) runTurn(ctx context.Context, work ingestWork) error {
	release, err := run.jobs.turn(ctx, work.kind())
	if err != nil {
		return err
	}
	defer release()
	return run.runWork(ctx, work)
}

// runWork runs a unit of work through its phases. A failure counts against its targets, unless the
// job ended under it: another runner took the lock, the process is stopping, or the watchdog ended a
// phase that was making progress, slowly, rather than stuck.
func (run *jobRun) runWork(ctx context.Context, work ingestWork) error {
	phases := []struct {
		name string
		run  func(context.Context) error
	}{
		{"fetch", work.fetch},
		{"build", work.build},
		{"publish", work.publish},
	}
	for _, phase := range phases {
		run.startPhase()
		if err := phase.run(ctx); errors.Is(err, errWorkDone) {
			return nil
		} else if err != nil {
			cause := context.Cause(ctx)
			if cause == nil {
				run.countFailure(ctx, work.targets(), err)
			} else if errors.Is(cause, errNoProgress) && run.stalled() {
				run.countFailure(ctx, work.targets(), cause)
			}
			return fmt.Errorf("%s phase: %w", phase.name, err)
		}
		util.ReportProgress(ctx)
	}
	return nil
}

// countFailure counts a failure of the targets' work, failing those at the limit, so that a target
// the job cannot publish does not hold the room's job forever
func (run *jobRun) countFailure(ctx context.Context, targets []*types.IngestTarget, cause error) {
	// The work's context may have ended, as its watchdog ends it
	ctx = context.WithoutCancel(ctx)
	zerolog.Ctx(ctx).Warn().Err(cause).Int("targets", len(targets)).Msg("Ingest work failed")
	now := time.Now().UnixMilli()
	for _, target := range targets {
		before, err := run.jobs.db.Rooms.UpdateIngestTarget(ctx, run.roomID, target.Event.ID, func(target *types.IngestTarget) {
			target.Failures++
			target.FailedAt = now
		})
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to count an ingest target's failure")
			return
		} else if before != nil && before.Failures+1 >= maxTargetFailures {
			if err := run.fail(ctx, before, failureFromError(cause)); err != nil && !errors.Is(err, errWorkDone) {
				zerolog.Ctx(ctx).Err(err).Msg("Failed to fail an ingest target")
			}
		}
	}
}

// fail fails a target, if it still exists, and ends its work
func (run *jobRun) fail(ctx context.Context, target *types.IngestTarget, failure *types.IngestFailure) error {
	failed, err := run.jobs.db.Rooms.FailIngestTarget(ctx, run.roomID, target.Event.ID, failure)
	if err != nil {
		return err
	}
	run.untarget(target.Event.ID)
	if failed {
		zerolog.Ctx(ctx).Warn().
			Stringer("event_id", target.Event.ID).
			Str("kind", string(target.Kind)).
			Int("status", failure.Status).
			Str("errcode", failure.ErrCode).
			Str("error", failure.Message).
			Msg("Ingest target failed")
		if target.Kind == types.IngestTargetJoin {
			run.joinDone(ctx, target, failure)
		}
	}
	return errWorkDone
}

func (run *jobRun) failAll(ctx context.Context, targets []*types.IngestTarget, failure *types.IngestFailure) error {
	for _, target := range targets {
		if err := run.fail(ctx, target, failure); !errors.Is(err, errWorkDone) {
			return err
		}
	}
	return nil
}

func targetIDs(targets []*types.IngestTarget) []id.EventID {
	ids := make([]id.EventID, len(targets))
	for i, target := range targets {
		ids[i] = target.Event.ID
	}
	return ids
}
