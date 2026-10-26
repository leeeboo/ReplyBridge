package commentbridge

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

const ManualEngagementFeedbackText = "此评论来自 YouTube。YouTube 官方 API 不支持自动点赞，请打开上方链接手动操作。"

// Bridge coordinates adapters; every external write is claimed and persisted
// before it is attempted, so it is never retried automatically.
type Bridge struct {
	ErrorAlerts             *ErrorAlerts
	Store                   *Store
	Sources                 []CommentSource
	Notifier                Notifier
	YouTubeWebhook          Notifier
	CredentialAlertNotifier CredentialAlertNotifier
	Refreshers              map[Platform]CredentialRefresher
	QRNotifier              CredentialQRNotifier
	Repliers                map[Platform]CommentReplier
	Reactor                 Reactor
	// MessageReplier posts human-facing, in-thread instructions for manual
	// actions. It is deliberately separate from platform comment repliers.
	MessageReplier MessageReplier
	Engagers       map[Platform]CommentEngager
	Moderators     map[Platform]CommentModerator
	// TargetChatID is persisted with every successful notification so a
	// reaction on a message from a previously configured group cannot approve
	// a platform write after the service changes groups.
	TargetChatID        string
	YouTubeTargetChatID string
}

// CredentialRefreshOnce advances one durable QR-login stage. Only the poll
// stage performs a safe external read repeatedly; upload/send outcomes are
// persisted before returning and are never retried automatically when unknown.
func (b *Bridge) CredentialRefreshOnce(ctx context.Context) error {
	job, err := b.Store.ClaimCredentialRefresh(ctx)
	if err != nil || job == nil {
		return err
	}
	refresher := b.Refreshers[job.Platform]
	if refresher == nil {
		return b.finishCredentialRefreshFailure(ctx, job, JobFailed, errors.New("missing credential refresher"))
	}
	switch job.Stage {
	case CredentialRefreshStageGenerate:
		session, beginErr := refresher.BeginCredentialRefresh(ctx)
		if beginErr != nil {
			status := JobFailed
			if IsUnknownOutcome(beginErr) {
				status = JobUnknown
			}
			return b.finishCredentialRefreshFailure(ctx, job, status, beginErr)
		}
		return b.Store.MarkCredentialRefreshGenerated(ctx, job.ActionID, session)
	case CredentialRefreshStageUpload:
		if b.QRNotifier == nil {
			return b.finishCredentialRefreshFailure(ctx, job, JobFailed, errors.New("missing credential QR notifier"))
		}
		if credentialRefreshExpired(job) {
			return b.finishCredentialRefreshFailure(ctx, job, JobFailed, errors.New("credential refresh QR expired"))
		}
		stageActionID := credentialRefreshStageActionID(job, "upload")
		imageKey, uploadErr := b.QRNotifier.UploadCredentialQR(ctx, job.Platform, job.Session.URL, stageActionID)
		if uploadErr != nil {
			status := JobFailed
			if IsUnknownOutcome(uploadErr) {
				status = JobUnknown
			}
			markErr := b.Store.MarkCredentialRefreshUploadFailed(ctx, job.ActionID, status, uploadErr)
			if markErr != nil {
				return markErr
			}
			if status == JobUnknown {
				return nil
			}
			return b.replyCredentialRefreshFailure(ctx, job)
		}
		return b.Store.MarkCredentialRefreshUploaded(ctx, job.ActionID, imageKey)
	case CredentialRefreshStageSend:
		if b.QRNotifier == nil {
			return b.finishCredentialRefreshFailure(ctx, job, JobFailed, errors.New("missing credential QR notifier"))
		}
		stageActionID := credentialRefreshStageActionID(job, "send")
		messageID, sendErr := b.QRNotifier.SendCredentialQR(ctx, job.Platform, job.ImageKey, stageActionID)
		if sendErr != nil {
			if IsUnknownOutcome(sendErr) {
				// The QR may already be visible in Feishu. Continue polling the
				// same opaque session without fabricating a second message.
				return b.Store.MarkCredentialRefreshSendUnknown(ctx, job.ActionID)
			}
			if markErr := b.Store.MarkCredentialRefreshFailed(ctx, job.ActionID, JobFailed, sendErr); markErr != nil {
				return markErr
			}
			return b.replyCredentialRefreshFailure(ctx, job)
		}
		return b.Store.MarkCredentialRefreshSent(ctx, job.ActionID, messageID, b.TargetChatID)
	case CredentialRefreshStagePoll:
		if credentialRefreshExpired(job) {
			if markErr := b.Store.MarkCredentialRefreshPollState(ctx, job.ActionID, CredentialRefreshExpired); markErr != nil {
				return markErr
			}
			return b.replyCredentialRefreshFailure(ctx, job)
		}
		state, pollErr := refresher.PollCredentialRefresh(ctx, job.Session.Key)
		if pollErr != nil {
			status := JobFailed
			if IsUnknownOutcome(pollErr) {
				status = JobUnknown
			}
			if markErr := b.Store.MarkCredentialRefreshPollFailed(ctx, job.ActionID, status, pollErr); markErr != nil {
				return markErr
			}
			if status == JobUnknown {
				return nil
			}
			return b.replyCredentialRefreshFailure(ctx, job)
		}
		switch state {
		case CredentialRefreshWaiting, CredentialRefreshScanned:
			return b.Store.MarkCredentialRefreshPollState(ctx, job.ActionID, state)
		case CredentialRefreshSucceeded:
			if err := b.Store.RecordCredentialSuccess(ctx, job.Platform, job.AccountID); err != nil {
				return err
			}
			if err := b.Store.MarkCredentialRefreshPollState(ctx, job.ActionID, state); err != nil {
				return err
			}
			return b.replyCredentialRefreshSuccess(ctx, job)
		case CredentialRefreshExpired:
			if err := b.Store.MarkCredentialRefreshPollState(ctx, job.ActionID, state); err != nil {
				return err
			}
			return b.replyCredentialRefreshFailure(ctx, job)
		default:
			return b.finishCredentialRefreshFailure(ctx, job, JobFailed, errors.New("invalid credential refresh state"))
		}
	default:
		return b.finishCredentialRefreshFailure(ctx, job, JobFailed, errors.New("invalid credential refresh stage"))
	}
}

func credentialRefreshExpired(job *CredentialRefreshJob) bool {
	return job != nil && !job.Session.ExpiresAt.IsZero() && !time.Now().Before(job.Session.ExpiresAt)
}

func credentialRefreshStageActionID(job *CredentialRefreshJob, stage string) string {
	return actionID("credential-refresh", stage, job.ActionID, strconv.Itoa(job.Attempt))
}

func (b *Bridge) finishCredentialRefreshFailure(ctx context.Context, job *CredentialRefreshJob, status JobStatus, err error) error {
	if markErr := b.Store.MarkCredentialRefreshFailed(ctx, job.ActionID, status, err); markErr != nil {
		return markErr
	}
	if status == JobUnknown {
		return nil
	}
	return b.replyCredentialRefreshFailure(ctx, job)
}

func (b *Bridge) replyCredentialRefreshSuccess(ctx context.Context, job *CredentialRefreshJob) error {
	if b.MessageReplier == nil || strings.TrimSpace(job.QRMessageID) == "" {
		return nil
	}
	_, err := b.MessageReplier.ReplyToMessage(ctx, job.QRMessageID, CredentialRefreshSuccessText, actionID("credential-refresh-success", job.ActionID))
	return err
}

func (b *Bridge) replyCredentialRefreshFailure(ctx context.Context, job *CredentialRefreshJob) error {
	if b.MessageReplier == nil || strings.TrimSpace(job.QRMessageID) == "" {
		return nil
	}
	_, err := b.MessageReplier.ReplyToMessage(ctx, job.QRMessageID, CredentialRefreshFailureText, actionID("credential-refresh-failure", job.ActionID))
	return err
}

func (b *Bridge) PollOnce(ctx context.Context) error {
	var errs []error
	for _, src := range b.Sources {
		cs, e := src.ListRecent(ctx)
		if b.ErrorAlerts != nil {
			b.ErrorAlerts.Observe(ctx, "poll_"+string(src.Platform()), e)
		}
		if e != nil {
			if alertErr := b.recordCredentialInvalid(ctx, src.Platform(), src.AccountID(), e); alertErr != nil {
				errs = append(errs, alertErr)
			}
			errs = append(errs, e)
			continue
		}
		if e = b.Store.RecordPoll(ctx, src, cs); e != nil {
			errs = append(errs, e)
		}
	}
	return errors.Join(errs...)
}

func (b *Bridge) recordCredentialInvalid(ctx context.Context, platform Platform, accountID string, err error) error {
	if !IsCredentialInvalid(err) {
		return nil
	}
	_, recordErr := b.Store.RecordCredentialInvalid(ctx, platform, accountID, CredentialInvalidReasonOf(err))
	return recordErr
}

// CredentialAlertOnce drains one durable credential-invalid alert. Alert
// delivery is itself an external write; failed and unknown outcomes remain
// terminal until an operator explicitly resets the job.
func (b *Bridge) CredentialAlertOnce(ctx context.Context) error {
	job, err := b.Store.ClaimCredentialAlert(ctx)
	if err != nil || job == nil {
		return err
	}
	if b.CredentialAlertNotifier == nil {
		if markErr := b.Store.MarkCredentialAlertFailed(ctx, job.ActionID, JobFailed, errors.New("missing credential alert notifier")); markErr != nil {
			b.bestEffortCredentialAlertUnknown(ctx, job.ActionID)
			return markErr
		}
		return nil
	}
	externalID, err := b.CredentialAlertNotifier.NotifyCredentialInvalid(ctx, job.Platform, job.ActionID)
	if err != nil {
		status := JobFailed
		if IsUnknownOutcome(err) {
			status = JobUnknown
		}
		if markErr := b.Store.MarkCredentialAlertFailed(ctx, job.ActionID, status, err); markErr != nil {
			b.bestEffortCredentialAlertUnknown(ctx, job.ActionID)
			return markErr
		}
		return nil
	}
	if markErr := b.Store.MarkCredentialAlertSent(ctx, job.ActionID, externalID); markErr != nil {
		b.bestEffortCredentialAlertUnknown(ctx, job.ActionID)
		return markErr
	}
	return nil
}

func (b *Bridge) NotifyOnce(ctx context.Context) error {
	j, e := b.Store.ClaimNotification(ctx)
	if e != nil || j == nil {
		return e
	}
	notifier := b.Notifier
	webhook := j.Comment.Platform == PlatformYouTube && b.YouTubeWebhook != nil && b.YouTubeTargetChatID == ""
	if webhook {
		notifier = b.YouTubeWebhook
	}
	id, e := notifier.NotifyComment(ctx, j.Comment, j.ActionID)
	if e != nil {
		st := JobFailed
		if IsUnknownOutcome(e) {
			st = JobUnknown
		}
		if markErr := b.Store.MarkNotificationFailed(ctx, j.ActionID, st, e); markErr != nil {
			b.bestEffortNotificationUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	var markErr error
	if webhook {
		markErr = b.Store.MarkWebhookNotificationSent(ctx, j.ActionID)
	} else {
		chatID := b.TargetChatID
		if j.Comment.Platform == PlatformYouTube && b.YouTubeTargetChatID != "" {
			chatID = b.YouTubeTargetChatID
		}
		markErr = b.Store.MarkNotificationSent(ctx, j.ActionID, id, chatID)
	}
	if markErr != nil {
		b.bestEffortNotificationUnknown(ctx, j.ActionID)
		return markErr
	}
	return nil
}
func (b *Bridge) AcceptReply(ctx context.Context, in IncomingReply) (bool, error) {
	if handled, accepted, err := b.Store.acceptModerationConfirmation(ctx, in); handled || err != nil {
		return accepted, err
	}
	return b.Store.AcceptReply(ctx, in)
}
func (b *Bridge) AcceptEngagement(ctx context.Context, in IncomingEngagement) (bool, error) {
	if moderationKind(in.ReactionType) != "" {
		if !b.acceptsCommandChat(in.ExpectedChatID) {
			return false, ErrUnmappedEngagement
		}
		return b.Store.AcceptModeration(ctx, in)
	}
	return b.Store.AcceptEngagement(ctx, in)
}
func (b *Bridge) ReplyOnce(ctx context.Context) error {
	j, e := b.Store.ClaimReply(ctx)
	if e != nil || j == nil {
		return e
	}
	r := b.Repliers[j.Comment.Platform]
	if r == nil {
		if markErr := b.Store.MarkReplyFailed(ctx, j.ActionID, JobFailed, errors.New("missing replier")); markErr != nil {
			b.bestEffortReplyUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	id, e := r.Reply(ctx, j.Comment, j.Text)
	if e != nil {
		credentialErr := b.recordCredentialInvalid(ctx, j.Comment.Platform, j.Comment.AccountID, e)
		st := JobFailed
		if IsUnknownOutcome(e) {
			st = JobUnknown
		}
		if markErr := b.Store.MarkReplyFailed(ctx, j.ActionID, st, e); markErr != nil {
			b.bestEffortReplyUnknown(ctx, j.ActionID)
			return markErr
		}
		if credentialErr != nil {
			return credentialErr
		}
		return nil
	}
	if markErr := b.Store.MarkReplySent(ctx, j.ActionID, id); markErr != nil {
		b.bestEffortReplyUnknown(ctx, j.ActionID)
		return markErr
	}
	if j.Comment.Platform == PlatformBilibili {
		if err := b.Store.RecordCredentialSuccess(ctx, j.Comment.Platform, j.Comment.AccountID); err != nil {
			return err
		}
	}
	return nil
}

// ReactionOnce processes one independently durable Feishu reaction. A
// reaction can never change the outcome of its associated platform reply.
func (b *Bridge) ReactionOnce(ctx context.Context) error {
	j, e := b.Store.ClaimReaction(ctx)
	if e != nil || j == nil {
		return e
	}
	if b.Reactor == nil {
		if markErr := b.Store.MarkReactionFailed(ctx, j.ActionID, JobFailed, errors.New("missing reactor")); markErr != nil {
			b.bestEffortReactionUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	id, e := b.Reactor.AddReaction(ctx, j.MessageID, j.EmojiType, j.ActionID)
	if e != nil {
		st := JobFailed
		if IsUnknownOutcome(e) {
			st = JobUnknown
		}
		if markErr := b.Store.MarkReactionFailed(ctx, j.ActionID, st, e); markErr != nil {
			b.bestEffortReactionUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	if markErr := b.Store.MarkReactionSent(ctx, j.ActionID, id); markErr != nil {
		b.bestEffortReactionUnknown(ctx, j.ActionID)
		return markErr
	}
	return nil
}

// EngagementOnce executes one platform engagement. A missing adapter is a
// terminal failed result because no automatic path exists for that platform.
func (b *Bridge) EngagementOnce(ctx context.Context) error {
	j, err := b.Store.ClaimEngagement(ctx)
	if err != nil || j == nil {
		return err
	}
	engager := b.Engagers[j.Comment.Platform]
	if engager == nil {
		if markErr := b.Store.MarkEngagementFailed(ctx, j.ActionID, JobFailed, errors.New("missing engager")); markErr != nil {
			b.bestEffortEngagementUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	id, err := engager.Engage(ctx, j.Comment, j.Kind)
	if err != nil {
		credentialErr := b.recordCredentialInvalid(ctx, j.Comment.Platform, j.Comment.AccountID, err)
		status := JobFailed
		if IsUnknownOutcome(err) {
			status = JobUnknown
		}
		if markErr := b.Store.MarkEngagementFailed(ctx, j.ActionID, status, err); markErr != nil {
			b.bestEffortEngagementUnknown(ctx, j.ActionID)
			return markErr
		}
		if credentialErr != nil {
			return credentialErr
		}
		return nil
	}
	if markErr := b.Store.MarkEngagementSent(ctx, j.ActionID, id); markErr != nil {
		b.bestEffortEngagementUnknown(ctx, j.ActionID)
		return markErr
	}
	if j.Comment.Platform == PlatformBilibili {
		if err := b.Store.RecordCredentialSuccess(ctx, j.Comment.Platform, j.Comment.AccountID); err != nil {
			return err
		}
	}
	return nil
}

// EngagementFeedbackOnce sends the OK receipt for an already successful
// engagement. Its terminal state never changes the engagement result.
func (b *Bridge) EngagementFeedbackOnce(ctx context.Context) error {
	j, err := b.Store.ClaimEngagementFeedback(ctx)
	if err != nil || j == nil {
		return err
	}
	if b.Reactor == nil {
		if markErr := b.Store.MarkEngagementFeedbackFailed(ctx, j.ActionID, JobFailed, errors.New("missing reactor")); markErr != nil {
			b.bestEffortEngagementFeedbackUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	id, err := b.Reactor.AddReaction(ctx, j.MessageID, j.EmojiType, j.ActionID)
	if err != nil {
		status := JobFailed
		if IsUnknownOutcome(err) {
			status = JobUnknown
		}
		if markErr := b.Store.MarkEngagementFeedbackFailed(ctx, j.ActionID, status, err); markErr != nil {
			b.bestEffortEngagementFeedbackUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	if markErr := b.Store.MarkEngagementFeedbackSent(ctx, j.ActionID, id); markErr != nil {
		b.bestEffortEngagementFeedbackUnknown(ctx, j.ActionID)
		return markErr
	}
	return nil
}

// ManualEngagementFeedbackOnce posts the fixed YouTube manual-like
// instruction under the notification that received the thumbs-up. It never
// invokes a platform API, and its terminal result never changes the original
// engagement event ledger.
func (b *Bridge) ManualEngagementFeedbackOnce(ctx context.Context) error {
	j, err := b.Store.ClaimManualEngagementFeedback(ctx)
	if err != nil || j == nil {
		return err
	}
	if b.MessageReplier == nil {
		if markErr := b.Store.MarkManualEngagementFeedbackFailed(ctx, j.ActionID, JobFailed, errors.New("missing message replier")); markErr != nil {
			b.bestEffortManualEngagementFeedbackUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	id, err := b.MessageReplier.ReplyToMessage(ctx, j.MessageID, ManualEngagementFeedbackText, j.ActionID)
	if err != nil {
		status := JobFailed
		if IsUnknownOutcome(err) {
			status = JobUnknown
		}
		if markErr := b.Store.MarkManualEngagementFeedbackFailed(ctx, j.ActionID, status, err); markErr != nil {
			b.bestEffortManualEngagementFeedbackUnknown(ctx, j.ActionID)
			return markErr
		}
		return nil
	}
	if markErr := b.Store.MarkManualEngagementFeedbackSent(ctx, j.ActionID, id); markErr != nil {
		b.bestEffortManualEngagementFeedbackUnknown(ctx, j.ActionID)
		return markErr
	}
	return nil
}

// A cancelled request context or a transient local write error must not leave
// an already-attempted external action looking retryable. These fallbacks use
// a fresh, short local context and only move sending -> unknown. If storage is
// unavailable they may also fail; startup recovery remains the final guard.
func (b *Bridge) bestEffortNotificationUnknown(ctx context.Context, actionID string) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = b.Store.MarkNotificationFailed(recoveryCtx, actionID, JobUnknown, errors.New("terminal persistence failed"))
}

func (b *Bridge) bestEffortCredentialAlertUnknown(ctx context.Context, actionID string) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = b.Store.MarkCredentialAlertFailed(recoveryCtx, actionID, JobUnknown, errors.New("terminal persistence failed"))
}

func (b *Bridge) bestEffortReplyUnknown(ctx context.Context, actionID string) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = b.Store.MarkReplyFailed(recoveryCtx, actionID, JobUnknown, errors.New("terminal persistence failed"))
}

func (b *Bridge) bestEffortReactionUnknown(ctx context.Context, actionID string) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = b.Store.MarkReactionFailed(recoveryCtx, actionID, JobUnknown, errors.New("terminal persistence failed"))
}

func (b *Bridge) bestEffortEngagementUnknown(ctx context.Context, actionID string) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = b.Store.MarkEngagementFailed(recoveryCtx, actionID, JobUnknown, errors.New("terminal persistence failed"))
}

func (b *Bridge) bestEffortEngagementFeedbackUnknown(ctx context.Context, actionID string) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = b.Store.MarkEngagementFeedbackFailed(recoveryCtx, actionID, JobUnknown, errors.New("terminal persistence failed"))
}

func (b *Bridge) bestEffortManualEngagementFeedbackUnknown(ctx context.Context, actionID string) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = b.Store.MarkManualEngagementFeedbackFailed(recoveryCtx, actionID, JobUnknown, errors.New("terminal persistence failed"))
}
