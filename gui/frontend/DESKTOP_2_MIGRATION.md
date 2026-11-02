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
| Workflow list, dry-run and run | V2 basic | editing and replay status |
| Provider setup / probe / admission | V2 active | secure credential path / E2E |
| Skills, MCP, memory, commands, hooks | V2 basic | granular permissions / tests |
| Token totals from Run Evidence | V2 basic | pricing and model allocation |
| Session rename, pin, archive, fork | Not migrated | preserve legacy tests |
| Full file preview and open in editor | Not migrated | independent V2 file pane |
| React 19 / Tailwind 4 / HeroUI v3 | Not migrated | compatibility gate |
| Wails real macOS/Windows packaging | Not verified | physical test |

## Deletion state
- The legacy src/styles.css was deleted because src/main.tsx now imports src/v2/tokens.css only.
- Legacy src/App.tsx and src/components/* are still imported by existing test suites; do not delete before test parity.
- The src/desktop/* bridge, Go runtime, Event Journal, permissions and frozen configuration are shared runtime contracts, not legacy UI.

## Safety invariants
1. No automatic approval: operation fingerprint comes from Journal.
2. No blind retry in waiting_reconcile; explicit verdict/evidence or Abort first.
3. Queue submit is not Queue confirm; Steering is separate.
4. Provider probe is not admission; user confirms bound evidence fingerprints.
5. Unknown provider pricing cannot be represented as an exact cost.
6. CI frontend/gate/gui plus live Wails and visual regression are required before the PR leaves Draft.
7. Do not delete legacy tests merely to hide feature regressions.
