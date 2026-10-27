package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

// BabbleBot's notices to a joining user. Their transaction IDs derive from the join event, so client
// retries, other processes and overlapping runners send each once.
func joinNoticeTxnID(eventID id.EventID) string  { return "join-pending/" + eventID.String() }
func joinOutcomeTxnID(eventID id.EventID) string { return "join-outcome/" + eventID.String() }

// noticeDelay is how long until a join enqueued at enqueued has been pending for after
func noticeDelay(enqueued int64, after time.Duration, now time.Time) time.Duration {
	return max(time.UnixMilli(enqueued).Add(after).Sub(now), 0)
}

func (run *jobRun) setRoomName(name string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if name != "" {
		run.roomName = name
	}
}

// roomDisplayName names the room by the name in the state before a join once fetched, else its ID
func (run *jobRun) roomDisplayName() string {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.roomName != "" {
		return run.roomName
	}
	return run.roomID.String()
}

// scheduleJoinNotices has BabbleBot tell each joining user that their join is under way, once it has
// been pending for JoinNoticeAfter
func (run *jobRun) scheduleJoinNotices(targets []*types.IngestTarget) {
	run.mu.Lock()
	defer run.mu.Unlock()
	after := run.jobs.config.BabbleBot.JoinNoticeAfter
	for _, target := range targets {
		if target.Kind != types.IngestTargetJoin || run.notices[target.Event.ID] != nil {
			continue
		}
		run.notices[target.Event.ID] = time.AfterFunc(noticeDelay(target.Enqueued, after, time.Now()), func() {
			run.sendJoinNotice(run.jobs.log.WithContext(run.jobs.ctx), target)
		})
	}
}

func (run *jobRun) sendJoinNotice(ctx context.Context, target *types.IngestTarget) {
	run.notice(ctx, target, joinNoticeTxnID(target.Event.ID), fmt.Sprintf(
		"Joining %s. This is a large room and can take a few minutes, you'll get a message here when it's ready.",
		run.roomDisplayName(),
	))
}

// cancelNotice stops the notice of a join that is no longer this run's to follow up: replaced, or
// done by another runner
func (run *jobRun) cancelNotice(eventID id.EventID) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if timer, found := run.notices[eventID]; found {
		timer.Stop()
		delete(run.notices, eventID)
	}
}

// joinDone follows up BabbleBot's notice of a join, if it was due, with the join's outcome
func (run *jobRun) joinDone(ctx context.Context, target *types.IngestTarget, failure *types.IngestFailure) {
	run.cancelNotice(target.Event.ID)
	if noticeDelay(target.Enqueued, run.jobs.config.BabbleBot.JoinNoticeAfter, time.Now()) > 0 {
		return
	}
	// Sent once whoever sends it, so the follow-up never precedes it
	run.sendJoinNotice(ctx, target)
	body := fmt.Sprintf("%s is ready.", run.roomDisplayName())
	if failure != nil {
		body = fmt.Sprintf("Joining %s failed: %s.", run.roomDisplayName(), failureReason(failure))
	}
	run.notice(ctx, target, joinOutcomeTxnID(target.Event.ID), body)
}

// failureReason says why a join failed in a notice, by its error code, so no server's error text
// reaches it
func failureReason(failure *types.IngestFailure) string {
	switch failure.ErrCode {
	case mautrix.MForbidden.ErrCode:
		return "you are not allowed to join it"
	case mautrix.MNotFound.ErrCode:
		return "no server could find it"
	case mautrix.MTooLarge.ErrCode:
		return "it is too large for this server"
	case mautrix.MUnsupportedRoomVersion.ErrCode:
		return "this server does not support its room version"
	}
	return "something went wrong, you can try joining it again"
}

func (run *jobRun) notice(ctx context.Context, target *types.IngestTarget, txnID, body string) {
	userID := id.UserID(*target.Event.StateKey)
	if err := run.jobs.bot.Notice(ctx, userID, txnID, body); err != nil {
		zerolog.Ctx(ctx).Err(err).Stringer("user_id", userID).Msg("Failed to send BabbleBot's notice of a join")
	}
}
