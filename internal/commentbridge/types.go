// Package commentbridge coordinates comment discovery, Feishu notification,
// and human-approved replies without coupling the platform adapters together.
package commentbridge

import (
	"context"
	"errors"
	"time"
)

type Platform string

const (
	PlatformBilibili Platform = "bilibili"
	PlatformYouTube  Platform = "youtube"
)

// Comment is the normalized, minimum record needed to notify a human and
// later reply to the same platform comment. It intentionally excludes raw API
// payloads and credentials.
type Comment struct {
	Platform  Platform
	AccountID string
	ID        string
	// RootID and ParentID retain the platform thread relationship. Reply
	// adapters resolve the target exclusively from this durable record; a
	// Feishu user can never supply platform IDs directly.
	RootID        string
	ParentID      string
	ResourceID    string
	ResourceTitle string
	ResourceURL   string
	AuthorID      string
	AuthorName    string
	Text          string
	PublishedAt   time.Time
	// IsChargePlus records Bilibili's explicit charging-member flag. It is
	// false for other platforms and for legacy rows where the flag was absent.
	IsChargePlus bool
}

// IncomingReply is a normalized Feishu message. Only ParentMessageID may
// resolve a target comment; RootMessageID is retained solely for audit context.
type IncomingReply struct {
	MessageID       string
	ParentMessageID string
	RootMessageID   string
	ChatID          string
	UserID          string
	Text            string
	ReceivedAt      time.Time
}

type CommentSource interface {
	Platform() Platform
	AccountID() string
	ListRecent(context.Context) ([]Comment, error)
}

type CommentReplier interface {
	Platform() Platform
	Reply(context.Context, Comment, string) (platformReplyID string, err error)
}

// EngagementKind identifies a platform action explicitly approved through a
// Feishu reaction.  It is deliberately separate from ReactionKind: the latter
// is an acknowledgement displayed in Feishu, while this is an external write.
type EngagementKind string

const (
	EngagementLike EngagementKind = "like"
)

// IncomingEngagement is the minimum immutable data from a Feishu reaction
// event. ReactionType is the provider value (for example, THUMBSUP). EmojiType
// is retained as a compatibility alias for adapters which call it that.
type IncomingEngagement struct {
	EventID   string
	MessageID string
	UserID    string
	// ExpectedChatID is the bridge's configured target, not a value supplied
	// by the Feishu reaction event (that event contains no chat_id). The store
	// compares it with the chat recorded when the bot notification was sent.
	ExpectedChatID string
	ReactionType   string
	EmojiType      string
	ReceivedAt     time.Time
}

// CommentEngager performs one explicitly approved platform engagement. The
// action ID is intentionally not exposed to platform adapters: engagement_jobs
// supplies durable de-duplication before an adapter is called.
type CommentEngager interface {
	Platform() Platform
	Engage(context.Context, Comment, EngagementKind) (externalID string, err error)
}

// Reactor adds a reaction to a Feishu message. The action ID is supplied as a
// durable idempotency/audit key for adapters that can make use of it.
type Reactor interface {
	AddReaction(ctx context.Context, messageID, emojiType, actionID string) (reactionID string, err error)
}

// MessageReplier posts a durable, human-facing reply beneath an existing
// Feishu message. It is intentionally narrower than CommentReplier: it can
// never receive a platform comment or write to an external platform.
type MessageReplier interface {
	ReplyToMessage(ctx context.Context, parentMessageID, text, actionID string) (messageID string, err error)
}

type Notifier interface {
	NotifyComment(context.Context, Comment, string) (feishuMessageID string, err error)
}

// CredentialAlertNotifier delivers a durable, operator-facing alert when a
// platform credential is rejected.  The action ID is the outbox idempotency
// key; adapters must not receive or persist the credential or the provider's
// raw error body.
type CredentialAlertNotifier interface {
	NotifyCredentialInvalid(context.Context, Platform, string) (externalID string, err error)
}

// CredentialRefreshSession is the short-lived, opaque login session returned
// by a platform's QR-login endpoint.  It deliberately contains no cookie or
// post-login URL; the bridge only persists the key and display URL until the
// refresh job reaches a terminal state.
type CredentialRefreshSession struct {
	Key       string
	URL       string
	ExpiresAt time.Time
}

// CredentialRefreshState is the result of one QR-login poll.
type CredentialRefreshState string

const (
	CredentialRefreshWaiting   CredentialRefreshState = "waiting"
	CredentialRefreshScanned   CredentialRefreshState = "scanned"
	CredentialRefreshSucceeded CredentialRefreshState = "succeeded"
	CredentialRefreshExpired   CredentialRefreshState = "expired"
	RefreshWaiting                                    = CredentialRefreshWaiting
	RefreshScanned                                    = CredentialRefreshScanned
	RefreshSucceeded                                  = CredentialRefreshSucceeded
	RefreshExpired                                    = CredentialRefreshExpired
)

// CredentialRefresher owns one platform's QR-login session.  Begin and Poll
// must not expose credentials in errors; the bridge stores only the opaque
// session key and the user-facing QR URL.
type CredentialRefresher interface {
	Platform() Platform
	BeginCredentialRefresh(context.Context) (CredentialRefreshSession, error)
	PollCredentialRefresh(context.Context, string) (CredentialRefreshState, error)
}

// CredentialQRNotifier uploads and sends a QR image to Feishu.  actionID is a
// stable durable idempotency key for the whole refresh incident, not a secret.
type CredentialQRNotifier interface {
	UploadCredentialQR(context.Context, Platform, string, string) (imageKey string, err error)
	SendCredentialQR(context.Context, Platform, string, string) (messageID string, err error)
}

const (
	CredentialRefreshRetryText   = "重新登录"
	CredentialRefreshSuccessText = "✅ B站登录已恢复，评论监控已继续。"
	CredentialRefreshFailureText = "二维码已过期或登录失败，请引用回复本二维码并发送“重新登录”获取新二维码。"
)

// CredentialRefreshStage records which operation is next.  The value is
// persisted separately from JobStatus so a restart can recover safe reads and
// leave external writes as unknown.
type CredentialRefreshStage string

const (
	CredentialRefreshStageGenerate CredentialRefreshStage = "generate"
	CredentialRefreshStageUpload   CredentialRefreshStage = "upload"
	CredentialRefreshStageSend     CredentialRefreshStage = "send"
	CredentialRefreshStagePoll     CredentialRefreshStage = "poll"
	RefreshStageGenerate                                  = CredentialRefreshStageGenerate
	RefreshStageUpload                                    = CredentialRefreshStageUpload
	RefreshStageSend                                      = CredentialRefreshStageSend
	RefreshStagePoll                                      = CredentialRefreshStagePoll
)

// ErrCredentialInvalid is a structured platform error sentinel.  Adapters
// must wrap this sentinel only when the provider explicitly says that the
// current credential/session is invalid; callers deliberately do not infer it
// from arbitrary error text, HTTP status, rate limits, or risk-control errors.
var ErrCredentialInvalid = errors.New("platform credential is invalid")

// CredentialInvalidReason is intentionally a closed set.  A platform adapter
// must select a reason from its structured response; arbitrary error text is
// never parsed into one.  Login and CSRF failures have different recovery
// rules in the durable source-health state machine.
type CredentialInvalidReason string

const (
	CredentialInvalidLogin   CredentialInvalidReason = "login"
	CredentialInvalidCSRF    CredentialInvalidReason = "csrf"
	CredentialInvalidUnknown CredentialInvalidReason = "unknown"
)

// CredentialInvalidError carries the structured provider reason while
// retaining errors.Is(err, ErrCredentialInvalid) compatibility for adapters
// and callers that only need the broad classification.
type CredentialInvalidError struct {
	Reason CredentialInvalidReason
}

func (e CredentialInvalidError) Error() string {
	if e.Reason == "" {
		return ErrCredentialInvalid.Error()
	}
	return string(ErrCredentialInvalid.Error()) + ": " + string(e.Reason)
}

func (e CredentialInvalidError) Unwrap() error { return ErrCredentialInvalid }

func NewCredentialInvalidError(reason CredentialInvalidReason) error {
	if reason == "" {
		reason = CredentialInvalidUnknown
	}
	return CredentialInvalidError{Reason: reason}
}

func CredentialInvalidReasonOf(err error) CredentialInvalidReason {
	var typed CredentialInvalidError
	if errors.As(err, &typed) && typed.Reason != "" {
		return typed.Reason
	}
	var typedPointer *CredentialInvalidError
	if errors.As(err, &typedPointer) && typedPointer != nil && typedPointer.Reason != "" {
		return typedPointer.Reason
	}
	return CredentialInvalidUnknown
}

func IsCredentialInvalid(err error) bool {
	return err != nil && errors.Is(err, ErrCredentialInvalid)
}

// UnknownOutcome marks an error from an external write whose request may have
// reached the platform. These actions must never be retried automatically.
type UnknownOutcome interface {
	error
	OutcomeUnknown() bool
}

func IsUnknownOutcome(err error) bool {
	var unknown UnknownOutcome
	return errors.As(err, &unknown) && unknown.OutcomeUnknown()
}

type JobStatus string

const (
	JobPending JobStatus = "pending"
	JobSending JobStatus = "sending"
	JobSent    JobStatus = "sent"
	JobFailed  JobStatus = "failed"
	JobUnknown JobStatus = "unknown"
)

type NotificationJob struct {
	ActionID string
	Comment  Comment
	Status   JobStatus
}

// CredentialAlertJob is the body-free durable outbox item for one credential
// invalid incident. IncidentSeq changes only after the source has been
// re-armed, so repeated failures in one incident produce one alert.
type CredentialAlertJob struct {
	ActionID    string
	Platform    Platform
	AccountID   string
	Reason      CredentialInvalidReason
	IncidentSeq int64
	Status      JobStatus
}

// CredentialRefreshJob is the durable QR-login state machine for one
// credential-invalid incident.  Session is cleared as soon as the job reaches
// sent/failed; QRMessageID is retained only so the bridge can post a terminal
// explanation beneath the QR message or accept an explicit retry reply.
type CredentialRefreshJob struct {
	ActionID      string
	Platform      Platform
	AccountID     string
	IncidentSeq   int64
	Stage         CredentialRefreshStage
	Attempt       int
	Session       CredentialRefreshSession
	ImageKey      string
	QRMessageID   string
	ChatID        string
	NextPollAt    time.Time
	ErrorCategory string
	Status        JobStatus
}

type ReplyJob struct {
	ActionID        string
	IncomingMessage string
	FeishuMessageID string
	Comment         Comment
	Text            string
	Status          JobStatus
}

type ReactionKind string

const (
	ReactionRead ReactionKind = "read"
	ReactionOK   ReactionKind = "ok"

	StatusReading = "StatusReading"
	OK            = "OK"
)

// ReactionJob is a separately durable Feishu reaction action. It deliberately
// carries no reply/comment body, so reaction delivery cannot affect platform
// reply delivery.
type ReactionJob struct {
	ActionID        string
	IncomingMessage string
	Kind            ReactionKind
	MessageID       string
	EmojiType       string
	Status          JobStatus
}

// EngagementJob is a durable external engagement action. It contains only the
// normalized comment needed by an adapter; Feishu actor/event data is never
// passed on to a platform.
type EngagementJob struct {
	ActionID        string
	FeishuMessageID string
	Comment         Comment
	Kind            EngagementKind
	Status          JobStatus
}

// EngagementFeedbackJob is the independently durable Feishu OK receipt for a
// completed engagement. It does not depend on inbox or reaction_jobs.
type EngagementFeedbackJob struct {
	ActionID           string
	EngagementActionID string
	MessageID          string
	EmojiType          string
	Status             JobStatus
}

// ManualEngagementFeedbackJob is a durable Feishu instruction explaining why
// an explicitly approved engagement needs a human action. It contains no
// operator identity or message body.
type ManualEngagementFeedbackJob struct {
	ActionID  string
	MessageID string
	Platform  Platform
	AccountID string
	CommentID string
	Kind      EngagementKind
	Status    JobStatus
}

var (
	ErrUnmappedReply = errors.New("feishu reply does not reference a mapped bot message")
	ErrInvalidReply  = errors.New("feishu reply text is invalid")
	// ErrUnsupportedEngagement means a mapped comment's platform deliberately
	// has no automatic engagement implementation. The caller may surface a
	// manual-action path, but no platform job is created.
	ErrUnsupportedEngagement = errors.New("engagement requires manual handling for this platform")
	ErrManualEngagement      = ErrUnsupportedEngagement
	ErrInvalidEngagement     = errors.New("feishu engagement is invalid")
	ErrUnmappedEngagement    = errors.New("feishu engagement does not reference a mapped bot message")
)

// ReplySink is implemented by the durable bridge. The bool reports whether a
// new reply action was enqueued; duplicate Feishu deliveries return false.
type ReplySink interface {
	AcceptReply(context.Context, IncomingReply) (bool, error)
}

// EngagementSink accepts a Feishu reaction event. The bool reports whether it
// created the one platform action for the mapped comment; separate users may
// still produce distinct durable events but never duplicate that action.
type EngagementSink interface {
	AcceptEngagement(context.Context, IncomingEngagement) (bool, error)
}
