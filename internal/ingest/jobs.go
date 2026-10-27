// Package ingest runs rooms' ingest jobs, which fetch, stage, build and publish their targets: joins,
// and PDUs arriving while the room has a job. See docs/event-sending.md#ingest-jobs.

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
	errWorkDone = errors.New("the work's targets are done")
	errWorkHeld = errors.New("the job held back work that failed")
	// Causes of ending a job's context
	errLeaseLost  = errors.New("the job's lock lease was lost")
	errNoProgress = errors.New("the job made no progress for its lease timeout")
)

const maxTargetFailures = 3

// Retries failed work and errored jobs sooner than the IngestJobWorker's scan
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

	mu      sync.Mutex
	running map[id.RoomID]*roomRunner
	// Bounds how many joins and gap fills the process runs at once
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

// turn reports progress while waiting, so the job keeps its lease
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

// reportProgressEvery keeps a job's lease through a wait bounded otherwise
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

// forget covers a runner that panicked before removing itself
func (j *Jobs) forget(roomID id.RoomID, runner *roomRunner) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running[roomID] == runner {
		delete(j.running, roomID)
	}
}

// runRoom reruns a finished job when a target was written since, as its writer may have failed to
// take the lock this runner held. A job holding back work reruns at once when asked to; else it, and
// a job stopped by an error, retries after a pause.
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

// runOnce reports whether the job ran until it had no targets left
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

// jobRun lives while its runner holds the room's lock
type jobRun struct {
	jobs        *Jobs
	roomID      id.RoomID
	roomVersion string
	lock        lock.Lock
	cancel      context.CancelCauseFunc
	// The round's unpublished targets, and how many state DAG events gap fills may still fetch
	targets   map[id.EventID]struct{}
	gapBudget int

	mu         sync.Mutex
	refreshed  time.Time
	watchdog   *time.Timer
	progressed bool
	roomName   string
	notices    map[id.EventID]*time.Timer
}

// progress refreshes the lease at most four times a lease. The watchdog stops the run shortly before
// an unrefreshed lease expires, and losing the lease stops it at once.
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

func (run *jobRun) watchdogTimeout() time.Duration {
	lease := run.jobs.config.Rooms.Ingest.LeaseTimeout
	return lease - lease/10
}

func (run *jobRun) startPhase() {
	run.mu.Lock()
	defer run.mu.Unlock()
	run.progressed = false
}

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

func (run *jobRun) ended(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// round holds back failed work, and work whose targets failed within the retry pause or cite a held
// target, until a round after the pause, as asking again at once could fail targets within seconds.
// A join can take minutes, so the round ends after one and the next puts PDUs written meanwhile first.
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

// untarget lets other targets' gap fills reach these as published, or as missing
func (run *jobRun) untarget(eventIDs ...id.EventID) {
	for _, eventID := range eventIDs {
		delete(run.targets, eventID)
	}
}

func (run *jobRun) runTurn(ctx context.Context, work ingestWork) error {
	release, err := run.jobs.turn(ctx, work.kind())
	if err != nil {
		return err
	}
	defer release()
	return run.runWork(ctx, work)
}

// runWork counts a failure against its targets unless the job ended under it, including the watchdog
// ending a phase that was slow rather than stuck
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

// countFailure fails targets at the limit, so one the job cannot publish does not hold it forever
func (run *jobRun) countFailure(ctx context.Context, targets []*types.IngestTarget, cause error) {
	// The watchdog may have ended the work's context
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
