package runtime

import (
	"context"
	"time"

	"github.com/toddzheng/llm-gateway/internal/core"
)

// Persistence ports are owned by the Response Runtime that consumes them;
// internal/store provides adapters and does not define cross-domain contracts.
type ResponseStore interface {
	Create(context.Context, string, core.Response) error
	Get(context.Context, string, string) (core.Response, error)
	Update(context.Context, string, core.Response, int64) error
	Delete(context.Context, string, string, int64) error
	ListInputItems(context.Context, string, string) ([]core.Item, error)
}

type IdempotentResponseStore interface {
	CreateIdempotent(context.Context, string, core.Response, string, string, []byte) (core.Response, bool, error)
}
type ConversationStore interface {
	CreateConversation(context.Context, string, core.Conversation) error
	GetConversation(context.Context, string, string) (core.Conversation, error)
	AppendConversationItems(context.Context, string, string, []core.Item, int64) (core.Conversation, error)
	DeleteConversation(context.Context, string, string, int64) error
}
type FinancialResponseFinalizer interface {
	FinalizeWithUsage(context.Context, string, core.Response, int64, core.UsageRecord) error
}
type GlobalQuotaStore interface {
	AcquireResponseSlot(context.Context, string, string, int, time.Time) error
	RenewResponseSlot(context.Context, string, string, time.Time) error
	ReleaseResponseSlot(context.Context, string, string) error
}
type RetentionStore interface {
	ScrubExpiredContent(context.Context, string, int) (int, error)
}
