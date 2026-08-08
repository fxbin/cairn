// Package app assembles the CLI-facing Cairn use cases.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/adapters/provider/openaicompat"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

var (
	ErrActiveRun        = errors.New("workspace already has an active run")
	ErrWaitingReconcile = errors.New("run has a dangling side-effect operation that requires reconciliation")
	ErrConfigDrift      = errors.New("frozen config fingerprint does not match current configuration")
	ErrStateCorrupt     = errors.New("run state is corrupt")
)

type EventSink func(domain.Event)

type Service struct {
	NewProvider func(config.Config) (ports.Provider, error)
}

type RunOptions struct {
	Task      string
	Workspace string
	StateDir  string
	Config    config.Config
	OnEvent   EventSink
}

type RunResult struct {
	ID       domain.RunID
	Response string
}

type ResumeOptions struct {
	RunID     domain.RunID
	Workspace string
	StateDir  string
	Config    config.Config
	Approval  agent.Approval
	OnEvent   EventSink
}

type ReconcileOptions struct {
	RunID       domain.RunID
	OperationID domain.OperationID
	Verdict     string
	Note        string
	Evidence    string
	Workspace   string
	StateDir    string
	Config      config.Config
	OnEvent     EventSink
}

// AbortOptions stops a running Run that has no uncertain external operation.
// A dangling workspace/process operation must instead be reconciled with the
// explicit abort_run verdict, preserving the evidence boundary.
type AbortOptions struct {
	RunID     domain.RunID
	Reason    string
	Workspace string
	StateDir  string
	Config    config.Config
	OnEvent   EventSink
}

type InspectResult struct {
	Projection domain.RunProjection `json:"projection"`
	Events     []domain.Event       `json:"events,omitempty"`
}

func (service Service) Run(ctx context.Context, options RunOptions) (RunResult, error) {
	if options.Task == "" {
		return RunResult{}, errors.New("task is required")
	}
	workspace, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	journal, err := openJournal(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer journal.Close()
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer workspaceLock.Release()
	active, err := journal.NonTerminalRuns(ctx, hash)
	if err != nil {
		return RunResult{}, err
	}
	if len(active) != 0 {
		switch active[0].Phase() {
		case "waiting_approval":
			pending := active[0].PendingApproval
			if pending != nil {
				return RunResult{}, fmt.Errorf("%w: %w", ErrActiveRun, &agent.WaitingApprovalError{RunID: active[0].ID, OperationID: pending.OperationID, Fingerprint: pending.Fingerprint, Summary: pending.Summary})
			}
		case "waiting_reconcile":
			return RunResult{}, fmt.Errorf("%w: %w", ErrActiveRun, ErrWaitingReconcile)
		}
		return RunResult{}, fmt.Errorf("%w: resume %s", ErrActiveRun, active[0].ID)
	}
	provider, err := service.provider(options.Config)
	if err != nil {
		return RunResult{}, err
	}
	artifacts, err := openArtifactStore(options.StateDir, hash, options.Config.Output)
	if err != nil {
		return RunResult{}, err
	}
	frozenConfig, err := json.Marshal(options.Config)
	if err != nil {
		return RunResult{}, fmt.Errorf("encode frozen run config: %w", err)
	}
	configRef, err := artifacts.Put(artifact.RedactJSON(frozenConfig), "application/json")
	if err != nil {
		return RunResult{}, fmt.Errorf("store frozen run config: %w", err)
	}
	runID := domain.NewRunID()
	first, err := journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunCreated, Payload: encode(struct {
		Task              string       `json:"task"`
		Workspace         string       `json:"workspace"`
		ConfigRef         artifact.Ref `json:"config_ref"`
		ConfigFingerprint string       `json:"config_fingerprint"`
	}{Task: options.Task, Workspace: workspace.Root, ConfigRef: configRef, ConfigFingerprint: options.Config.Fingerprint()})})
	if err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(first)
	}
	second, err := journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: hash, Type: domain.EventRunStarted, Payload: encode(struct {
		Invocation string `json:"invocation"`
	}{Invocation: domain.NewID("invoke")})})
	if err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(second)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	environmentAllow := make(map[string]struct{}, len(options.Config.Policy.EnvAllow))
	for _, key := range options.Config.Policy.EnvAllow {
		environmentAllow[key] = struct{}{}
	}
	grace, err := options.Config.TerminateGrace()
	if err != nil {
		return RunResult{}, err
	}
	execTool := tools.ExecCommand{Workspace: workspace, EnvAllow: environmentAllow, Grace: grace}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace}, patchTool, execTool)
	if err != nil {
		return RunResult{}, err
	}
	gate, err := policy.New(options.Config.Policy.Write, options.Config.Policy.Exec, options.Config.Policy.ProtectedPaths, patchTool)
	if err != nil {
		return RunResult{}, err
	}
	verification, err := verificationCommands(options.Config.Verification)
	if err != nil {
		return RunResult{}, err
	}
	loop := configuredLoop(journal, provider, registry, gate, verification, artifacts, options.Config, options.OnEvent)
	runContext, cancelRun, err := runBudgetContext(ctx, first.OccurredAt, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	defer cancelRun()
	result, err := loop.Run(runContext, agent.RunInput{RunID: runID, Task: options.Task, WorkspaceHash: hash})
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = loop.Run(expiredContext, agent.RunInput{RunID: runID, Task: options.Task, WorkspaceHash: hash})
	}
	if err != nil {
		return RunResult{ID: runID}, err
	}
	return RunResult{ID: result.RunID, Response: result.Response}, nil
}

func (service Service) Inspect(ctx context.Context, workspacePath, stateDir string, config config.Config, runID domain.RunID, includeEvents bool) (InspectResult, error) {
	_, hash, err := workspaceFor(workspacePath, config)
	if err != nil {
		return InspectResult{}, err
	}
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		return InspectResult{}, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return InspectResult{}, err
	}
	if len(events) == 0 {
		return InspectResult{}, fmt.Errorf("run %s not found", runID)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkArtifactReferences(ctx, journal, stateDir, hash); err != nil {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	result := InspectResult{Projection: projection}
	if includeEvents {
		result.Events = events
	}
	return result, nil
}

func (service Service) Resume(ctx context.Context, options ResumeOptions) (RunResult, error) {
	workspace, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	journal, err := openJournal(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer journal.Close()
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer workspaceLock.Release()
	events, err := journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.State != domain.RunRunning {
		return RunResult{ID: options.RunID}, errors.New("run is not resumable")
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkArtifactReferences(ctx, journal, options.StateDir, hash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=<verdict>", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	observation, err := workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	provider, err := service.provider(options.Config)
	if err != nil {
		return RunResult{}, err
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	environmentAllow := make(map[string]struct{}, len(options.Config.Policy.EnvAllow))
	for _, key := range options.Config.Policy.EnvAllow {
		environmentAllow[key] = struct{}{}
	}
	grace, err := options.Config.TerminateGrace()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	execTool := tools.ExecCommand{Workspace: workspace, EnvAllow: environmentAllow, Grace: grace}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace}, patchTool, execTool)
	if err != nil {
		return RunResult{}, err
	}
	gate, err := policy.New(options.Config.Policy.Write, options.Config.Policy.Exec, options.Config.Policy.ProtectedPaths, patchTool)
	if err != nil {
		return RunResult{}, err
	}
	verification, err := verificationCommands(options.Config.Verification)
	if err != nil {
		return RunResult{}, err
	}
	artifacts, err := openArtifactStore(options.StateDir, hash, options.Config.Output)
	if err != nil {
		return RunResult{}, err
	}
	loop := configuredLoop(journal, provider, registry, gate, verification, artifacts, options.Config, options.OnEvent)
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	defer cancelRun()
	resumeInput := agent.RunInput{RunID: options.RunID, WorkspaceHash: hash, ResumeInvocation: domain.NewID("invoke"), ResumeFromSeq: projection.LastSeq, ResumeObservation: observation}
	result, err := loop.Resume(runContext, resumeInput, options.Approval)
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		resumeInput.ResumeInvocation = ""
		result, err = loop.Resume(expiredContext, resumeInput, options.Approval)
	}
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	return RunResult{ID: result.RunID, Response: result.Response}, nil
}

// Reconcile writes the human verdict for an operation whose external effect
// began but whose terminal event was never persisted. It never re-executes the
// original command or patch.
func (service Service) Reconcile(ctx context.Context, options ReconcileOptions) (RunResult, error) {
	workspace, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	journal, err := openJournal(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer journal.Close()
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer workspaceLock.Release()
	events, err := journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkArtifactReferences(ctx, journal, options.StateDir, hash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.State != domain.RunRunning || projection.PendingOperation == nil || projection.PendingOperation.ID != options.OperationID {
		return RunResult{}, ErrWaitingReconcile
	}
	if projection.PendingOperation.SideEffectClass != "workspace" && projection.PendingOperation.SideEffectClass != "process" {
		return RunResult{}, errors.New("only workspace and process operations require reconciliation")
	}
	switch options.Verdict {
	case "applied_success", "applied_failure", "not_applied", "abort_run":
	default:
		return RunResult{}, errors.New("verdict must be applied_success, applied_failure, not_applied, or abort_run")
	}
	if strings.TrimSpace(options.Note) == "" {
		return RunResult{}, errors.New("reconciliation note is required")
	}
	if projection.PendingOperation.SideEffectClass == "process" && strings.TrimSpace(options.Evidence) == "" {
		return RunResult{}, errors.New("process reconciliation requires evidence")
	}
	if projection.PendingOperation.ToolName == "apply_patch" && projection.PendingOperation.PlannedPath != "" {
		observation, err := patchObservation(workspace, projection.PendingOperation.PlannedPath)
		if err != nil {
			return RunResult{}, err
		}
		switch options.Verdict {
		case "applied_success":
			if observation != projection.PendingOperation.PlannedAfter {
				return RunResult{}, fmt.Errorf("patch observation %s does not match planned after hash %s", observation, projection.PendingOperation.PlannedAfter)
			}
		case "not_applied":
			if observation != projection.PendingOperation.PlannedBefore {
				return RunResult{}, fmt.Errorf("patch observation %s does not match planned before hash %s", observation, projection.PendingOperation.PlannedBefore)
			}
		}
	}
	operationID := options.OperationID
	turnID := projection.CurrentTurn
	if turnID == nil {
		return RunResult{}, errors.New("dangling operation has no open turn")
	}
	appendEvent := func(eventType domain.EventType, payload any) error {
		event, err := journal.Append(ctx, projection.LastSeq, domain.EventDraft{RunID: options.RunID, WorkspaceHash: hash, Type: eventType, TurnID: turnID, OperationID: &operationID, Payload: encode(payload)})
		if err != nil {
			return err
		}
		if err := projection.Apply(event); err != nil {
			return err
		}
		if options.OnEvent != nil {
			options.OnEvent(event)
		}
		return nil
	}
	if err := appendEvent(domain.EventOperationReconciled, struct {
		Verdict              string `json:"verdict"`
		Actor                string `json:"actor"`
		Note                 string `json:"note"`
		Evidence             string `json:"evidence"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Verdict: options.Verdict, Actor: "human", Note: options.Note, Evidence: options.Evidence, WorkspaceObservation: workspace.Root}); err != nil {
		return RunResult{}, err
	}
	if options.Verdict == "abort_run" {
		if err := appendEvent(domain.EventTurnClosed, domain.TurnClosedPayload{Outcome: "failed"}); err != nil {
			return RunResult{}, err
		}
		if err := appendEvent(domain.EventRunFailed, struct {
			Error string `json:"error"`
		}{Error: "reconciliation aborted"}); err != nil {
			return RunResult{}, err
		}
		return RunResult{ID: options.RunID}, nil
	}
	if err := appendEvent(domain.EventToolResultAppended, tools.Result{OK: options.Verdict == "applied_success", Code: "reconciled_" + options.Verdict, Preview: options.Note}); err != nil {
		return RunResult{}, err
	}
	if err := appendEvent(domain.EventTurnClosed, domain.TurnClosedPayload{Outcome: "continue"}); err != nil {
		return RunResult{}, err
	}
	observation, err := workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	resumed, err := journal.Append(ctx, projection.LastSeq, domain.EventDraft{RunID: options.RunID, WorkspaceHash: hash, Type: domain.EventRunResumed, Payload: encode(struct {
		Invocation           string `json:"invocation"`
		FromSeq              uint64 `json:"from_seq"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Invocation: domain.NewID("invoke"), FromSeq: projection.LastSeq, WorkspaceObservation: observation})})
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := projection.Apply(resumed); err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(resumed)
	}
	provider, err := service.provider(options.Config)
	if err != nil {
		return RunResult{}, err
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	environmentAllow := make(map[string]struct{}, len(options.Config.Policy.EnvAllow))
	for _, key := range options.Config.Policy.EnvAllow {
		environmentAllow[key] = struct{}{}
	}
	grace, err := options.Config.TerminateGrace()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace}, patchTool, tools.ExecCommand{Workspace: workspace, EnvAllow: environmentAllow, Grace: grace})
	if err != nil {
		return RunResult{}, err
	}
	gate, err := policy.New(options.Config.Policy.Write, options.Config.Policy.Exec, options.Config.Policy.ProtectedPaths, patchTool)
	if err != nil {
		return RunResult{}, err
	}
	verification, err := verificationCommands(options.Config.Verification)
	if err != nil {
		return RunResult{}, err
	}
	artifacts, err := openArtifactStore(options.StateDir, hash, options.Config.Output)
	if err != nil {
		return RunResult{}, err
	}
	loop := configuredLoop(journal, provider, registry, gate, verification, artifacts, options.Config, options.OnEvent)
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	defer cancelRun()
	result, err := loop.Run(runContext, agent.RunInput{RunID: options.RunID, Task: projection.Task, WorkspaceHash: hash})
	if errors.Is(err, context.DeadlineExceeded) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = loop.Run(expiredContext, agent.RunInput{RunID: options.RunID, Task: projection.Task, WorkspaceHash: hash})
	}
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	return RunResult{ID: result.RunID, Response: result.Response}, nil
}

// Abort terminates a non-dangling run without starting any model, tool, or
// verifier work. It is intentionally separate from Reconcile: uncertainty
// about an already-started side effect can only be resolved through a verdict.
func (service Service) Abort(ctx context.Context, options AbortOptions) (RunResult, error) {
	if strings.TrimSpace(options.Reason) == "" {
		return RunResult{}, errors.New("abort reason is required")
	}
	_, hash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	journal, err := openJournal(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer journal.Close()
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, hash)
	if err != nil {
		return RunResult{}, err
	}
	defer workspaceLock.Release()
	events, err := journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.State != domain.RunRunning {
		return RunResult{ID: options.RunID}, errors.New("run is not abortable")
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkArtifactReferences(ctx, journal, options.StateDir, hash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.PendingOperation != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=abort_run", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	appendEvent := func(eventType domain.EventType, turnID *domain.TurnID, operationID *domain.OperationID, payload any) error {
		event, err := journal.Append(ctx, projection.LastSeq, domain.EventDraft{RunID: options.RunID, WorkspaceHash: hash, Type: eventType, TurnID: turnID, OperationID: operationID, Payload: encode(payload)})
		if err != nil {
			return err
		}
		if err := projection.Apply(event); err != nil {
			return err
		}
		if options.OnEvent != nil {
			options.OnEvent(event)
		}
		return nil
	}
	if pending := projection.PendingApproval; pending != nil {
		if projection.CurrentTurn == nil {
			return RunResult{}, errors.New("pending approval has no open turn")
		}
		turnID, operationID := *projection.CurrentTurn, pending.OperationID
		if err := appendEvent(domain.EventApprovalDenied, &turnID, &operationID, struct {
			Fingerprint string `json:"fingerprint"`
			Actor       string `json:"actor"`
			Reason      string `json:"reason"`
		}{Fingerprint: pending.Fingerprint, Actor: "system", Reason: options.Reason}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := appendEvent(domain.EventTurnClosed, &turnID, nil, domain.TurnClosedPayload{Outcome: "failed"}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	if err := appendEvent(domain.EventRunFailed, nil, nil, struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}{Error: "user_aborted", Reason: options.Reason}); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	return RunResult{ID: options.RunID}, nil
}

func (service Service) Doctor(ctx context.Context, workspacePath, stateDir string, config config.Config) error {
	_, hash, err := workspaceFor(workspacePath, config)
	if err != nil {
		return err
	}
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		return err
	}
	defer journal.Close()
	if err := journal.Check(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkArtifactReferences(ctx, journal, stateDir, hash); err != nil {
		return fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return nil
}

func (service Service) provider(configuration config.Config) (ports.Provider, error) {
	if service.NewProvider != nil {
		return service.NewProvider(configuration)
	}
	if configuration.Provider.BaseURL == "" || configuration.Provider.Model == "" {
		return nil, errors.New("provider base URL and model are required")
	}
	timeout, err := configuration.RequestTimeout()
	if err != nil {
		return nil, err
	}
	return openaicompat.Client{BaseURL: configuration.Provider.BaseURL, APIKey: os.Getenv(configuration.Provider.APIKeyEnv), HTTPClient: &http.Client{Timeout: timeout}}, nil
}

func workspaceFor(root string, configuration config.Config) (tools.Workspace, string, error) {
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return tools.Workspace{}, "", err
		}
		root = current
	}
	workspace, err := tools.NewWorkspace(root, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		return tools.Workspace{}, "", err
	}
	digest := sha256.Sum256([]byte(workspace.Root))
	return workspace, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func openJournal(stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.Open(filepath.Join(directory, "runs.db"))
}

func openArtifactStore(stateDir, workspaceHash string, output config.OutputConfig) (*artifact.Store, error) {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	store, err := artifact.New(filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts"), output.MaxPreviewBytes)
	if err != nil {
		return nil, err
	}
	store.MaxBytes = output.MaxArtifactBytes
	return store, nil
}

func checkArtifactReferences(ctx context.Context, journal *sqlite.Journal, stateDir, workspaceHash string) error {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	runIDs, err := journal.RunIDs(ctx)
	if err != nil {
		return err
	}
	for _, runID := range runIDs {
		events, err := journal.Load(ctx, runID)
		if err != nil {
			return err
		}
		for _, event := range events {
			var payload struct {
				ResultRef *artifact.Ref `json:"result_ref"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.ResultRef == nil {
				continue
			}
			if err := checkArtifactReference(filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts"), *payload.ResultRef); err != nil {
				return fmt.Errorf("run %s event %d: %w", runID, event.Seq, err)
			}
		}
	}
	return nil
}

func checkArtifactReference(root string, reference artifact.Ref) error {
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("invalid artifact digest %q", reference.SHA256)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("decode artifact digest: %w", err)
	}
	content, err := os.ReadFile(filepath.Join(root, "sha256", digest[:2], digest))
	if err != nil {
		return fmt.Errorf("read referenced artifact: %w", err)
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != digest {
		return errors.New("referenced artifact hash mismatch")
	}
	return nil
}

func openReadOnlyJournal(stateDir, workspaceHash string) (*sqlite.Journal, error) {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	directory := filepath.Join(stateDir, "workspaces", workspaceHash)
	return sqlite.OpenReadOnly(filepath.Join(directory, "runs.db"))
}

func acquireWorkspaceLock(stateDir, workspaceHash string) (*lock.Lock, error) {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	return lock.Acquire(filepath.Join(stateDir, "workspaces", workspaceHash, "run.lock"))
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".cairn"
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Cairn")
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "cairn")
	}
	return filepath.Join(home, ".local", "state", "cairn")
}

func encode(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return append(json.RawMessage(nil), artifact.RedactJSON(encoded)...)
}

func requireMatchingConfig(events []domain.Event, configuration config.Config) error {
	if len(events) == 0 || events[0].Type != domain.EventRunCreated {
		return errors.New("run is missing run_created event")
	}
	var payload struct {
		ConfigFingerprint string `json:"config_fingerprint"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		return fmt.Errorf("decode frozen run config: %w", err)
	}
	if payload.ConfigFingerprint == "" || payload.ConfigFingerprint != configuration.Fingerprint() {
		return ErrConfigDrift
	}
	return nil
}

func patchObservation(workspace tools.Workspace, relativePath string) (string, error) {
	path, _, err := workspace.Resolve(relativePath)
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func verificationCommands(configuration config.VerificationConfig) ([]agent.VerificationCommand, error) {
	commands := make([]agent.VerificationCommand, 0, len(configuration.Commands))
	for index, command := range configuration.Commands {
		timeout := 120 * time.Second
		if command.Timeout != "" {
			parsed, err := time.ParseDuration(command.Timeout)
			if err != nil {
				return nil, err
			}
			timeout = parsed
		}
		maximumOutput := command.MaxOutputBytes
		if maximumOutput == 0 {
			maximumOutput = 256 * 1024
		}
		arguments, err := json.Marshal(tools.ExecArguments{Argv: command.Argv, CWD: command.CWD, TimeoutMS: int(timeout.Milliseconds()), MaxOutputBytes: maximumOutput, Env: command.Env})
		if err != nil {
			return nil, err
		}
		commands = append(commands, agent.VerificationCommand{ID: fmt.Sprintf("verification-%d", index+1), Arguments: arguments})
	}
	return commands, nil
}

func configuredLoop(journal ports.Journal, provider ports.Provider, registry *tools.Registry, gate *policy.Gate, verification []agent.VerificationCommand, artifacts *artifact.Store, configuration config.Config, onEvent EventSink) agent.Loop {
	return agent.Loop{
		Journal: journal, Provider: provider, Tools: registry, Gate: gate,
		Model: configuration.Provider.Model, ConfigFingerprint: configuration.Fingerprint(),
		MaxTurns: configuration.Budget.MaxTurns, MaxToolCalls: configuration.Budget.MaxToolCalls,
		MaxInputTokens: configuration.Budget.MaxInputTokens, MaxOutputTokens: configuration.Budget.MaxOutputTokens,
		MaxCostUSD: configuration.Budget.MaxCostUSD, InputTokenUSD: configuration.Pricing.InputTokenUSD,
		OutputTokenUSD: configuration.Pricing.OutputTokenUSD,
		Verification:   verification, VerificationWorkspace: gate.Patch.Workspace.ObservationView(configuration.Verification.VolatilePaths),
		Artifacts: artifacts, OnEvent: agent.EventSink(onEvent),
	}
}

func runBudgetContext(ctx context.Context, createdAt time.Time, configuration config.Config) (context.Context, context.CancelFunc, error) {
	wallTime, err := configuration.WallTime()
	if err != nil {
		return nil, nil, err
	}
	budgetContext, cancel := context.WithDeadline(ctx, createdAt.Add(wallTime))
	return budgetContext, cancel, nil
}

func expiredWallBudgetContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, time.Now().Add(-time.Nanosecond))
}
