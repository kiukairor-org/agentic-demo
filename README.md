# app-template

Template for every app in `kiukairor-org` built by the homie-pipeline (idea in Mattermost → Plane →
GitHub issue → Claude PR → CI → independent review → auto-merge → Argo CD).

| Path | What |
|---|---|
| `main.go`, `main_test.go` | minimal Go service (`GET /`, `GET /healthz`) |
| `Dockerfile` | arm64 image (distroless, non-root) |
| `deploy/` | kustomize base; Argo CD sets namespace, image tag (= commit SHA) and tailnet hostname |
| `CLAUDE.md` | rules for the author agent |
| `.github/workflows/claude.yml` | author agent (issue labelled `agent` / `@claude` from owner or kiu-homie-bot) |
| `.github/workflows/ci.yml` | lint, test, arm64 image → GHCR; check `ci` is required; ≤ 2 agent fix rounds |
| `.github/workflows/review.yml` | independent reviewer (kiu-homie-reviewer) after green CI; escalation gate first |
| `.github/workflows/hold.yml` | label `hold` disables auto-merge |

Kill switch: org variable `AUTO_MERGE_ENABLED` ≠ `true` → no approvals, no auto-merge.
Labels used: `agent`, `preview`, `fix-round-1`, `fix-round-2`, `needs-human`, `hold`.
