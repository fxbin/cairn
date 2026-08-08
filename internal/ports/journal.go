package ports

import (
	"context"

	"github.com/fxbin/cairn/internal/domain"
)

type Journal interface {
	Append(ctx context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error)
	Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error)
	NonTerminalRuns(ctx context.Context, workspaceHash string) ([]domain.RunProjection, error)
	Check(ctx context.Context) error
}
