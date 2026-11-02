# Cairn Desktop 2.0 / Issue #33 — migration matrix

Scope: production entry uses V2 only; Go, Wails, Journal and Desktop Client remain unchanged.

| Contract | Status | Next gate |
|---|---|---|
| Composer / workspace / sessions | V2 active | native visual E2E |
| Journal transcript, approvals, cancel | V2 active | live run regression |
| Reconciliation & abort | V2 active | run with uncertain side-effect |
| Steering and explicit queue confirmation | V2 active | real local workflow |
| Subagent overview and navigation | Basic V2 | parent breadcrumb and tree |
| Diff and line review | V2 active | editor / binary / untracked cases |
| Evidence and trust | V2 read-only | evidence failure handling |
| Workflow list, edit, save, dry-run and run | V2 migrated | WorkflowsV2 + explicit gates; full run replay E2E pending |
| Provider setup / probe / admission | V2 active | secure credential path / E2E |
| Skills, MCP, memory, commands, hooks | V2 basic | granular permissions / tests |
| Token totals from Run Evidence | V2 basic | pricing and model allocation |
| Session rename, pin, archive, fork | V2 migrated | SessionRowV2 + AppV2 tests; native E2E pending |
| Full file preview and open in editor | V2 migrated | FilesPanelV2 + tests for text, binary, truncation, paths |
| React 19 / Tailwind 4 / HeroUI v3 | Not migrated | compatibility gate |
| Wails real macOS/Windows packaging | Not verified | physical test |

## Deletion state
- The legacy src/styles.css was deleted because src/main.tsx now imports src/v2/tokens.css only.
- Legacy src/App.tsx and all seven src/components/* files have been **physically deleted** on the feature branch after V2 parity tests were added.
- Five legacy UI test files were superseded by AppV2.test.tsx, V2Parity.test.tsx, and V2Capabilities.test.tsx. Transport, events, timeline, chatBlocks and type-scale tests remain.
- The src/desktop/* bridge, Go runtime, Event Journal, permissions and frozen configuration are shared runtime contracts, not legacy UI.

## Safety invariants
1. No automatic approval: operation fingerprint comes from Journal.
2. No blind retry in waiting_reconcile; explicit verdict/evidence or Abort first.
3. Queue submit is not Queue confirm; Steering is separate.
4. Provider probe is not admission; user confirms bound evidence fingerprints.
5. Unknown provider pricing cannot be represented as an exact cost.
6. CI frontend/gate/gui plus live Wails and visual regression are required before the PR leaves Draft.
7. Do not delete legacy tests merely to hide feature regressions.

## Legacy test-to-V2 parity mapping (2026-11-02)
- App.test.tsx → AppV2.test.tsx: home, Composer submit, provider setup covered with V2Parity Provider tests.
- interactions.test.tsx → AppV2.test.tsx: read-only, queued follow-up, Ctrl+Enter, draft restoration, approval fingerprint and retry, thread isolation, A→B→A protection.
- journal-poll.test.tsx → AppV2.test.tsx: late approval discovery via 2-second Journal poll.
- present-details.test.tsx → V2Parity.test.tsx: tree, preview metadata, truncated/binary content, editor targets; AppV2.test.tsx: dirty marker, bridge health, server Git counts.
- provider-settings.test.tsx → V2Parity.test.tsx: setup credential variable validation, pass/inconclusive distinction, evidence-bound admission, stale workspace responses.
- Additional V2Capabilities.test.tsx: workflow edit/execute, finite Hooks, explicit memory revoke and AGENTS.md authoring.
- Runtime tests for bridge isolation, event transport, timeline and journal projection are *retained* and unchanged.

## Post-deletion gates
- Typecheck + Vitest + production Vite build, Go gate and GUI build must pass on the **post-deletion commit**.
- Native macOS/Windows app run, icon/window behavior and 720px/960px/1580px layout inspection remain prerequisites to merge.
- Missing enhancement: full end-to-end trace for Workflow execution and all capability screens. This is a verification task, not a reason to restore unused legacy UI.
