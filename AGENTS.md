<!-- specgit:project:v2:start -->
## SpecGit 2

Read project-root `.agents/skills/specgit-native/SKILL.md` before work; if missing, initialize with the shared `specgit init` CLI or restore an existing integration with `specgit setup` ([installation](https://github.com/LeXwDeX/SpecGit/blob/main/docs/agent-install.md)).
<!-- specgit:project:v2:end -->

This file is the single instruction file for every coding agent working in this repository. There is no `CLAUDE.md`; tools that look for one should read this file.

## Project

Tribunal is a multi-agent code review service for GitLab CE merge requests: a Go server with an embedded React console, SQLite storage and Docker Compose deployment. MIT licensed. User-facing docs are in Chinese.

- Product goals and design baseline: `ARCH.md`, `PRODUCT.md`
- Detailed design: `docs/design/` (modules, data model, HTTP API, DAG, flows, config, web console)
- Operations: `docs/operations.md`. Agent-driven deployment and integration: `docs/agent-deployment.md`
- Release notes: `CHANGELOG.md`. Verification status: `docs/delivery-readiness.md`

## Layout

| Path | Contents |
|---|---|
| `cmd/server`, `cmd/tribunalctl` | Server entry point and admin CLI |
| `internal/server` | HTTP API, auth, CSRF, SSE, webhook endpoint, wiring |
| `internal/app` | Application services: projects, reviews, webhooks, extensions, workspaces, publishing |
| `internal/review`, `pkg/dag` | Review blocks and contracts; scenario-agnostic DAG engine |
| `internal/agent`, `internal/llm` | Agent tool loop and sandbox; OpenAI Chat, OpenAI Responses and Anthropic adapters |
| `internal/gitlab`, `internal/repo`, `internal/workspace` | GitLab client, git operations, per-project workspaces |
| `internal/mcp`, `internal/skill` | MCP client pool, Claude-compatible Skill loader, built-in skills |
| `internal/store`, `internal/store/sqlite`, `internal/store/migrations` | Store interfaces, SQLite implementation, numbered migrations |
| `internal/secret`, `internal/errclass`, `internal/config`, `internal/jobs` | Encryption, sanitized error classes, config loading, job queue |
| `prompts/` | Embedded block prompts and platform rules |
| `web/` | React 19 console; `web/dist` is the committed build embedded into the binary |
| `config/` | `server.example.yaml` and `server.docker.yaml` (mounted by Compose) |
| `scripts/` | Coverage and license checks, license bundle, Docker smoke, MCP wrapper |
| `prototype/v2/` | Visual prototype and `IMPLEMENT.md` mapping for the v2 console |
| `third_party/` | License and version records for bundled executables |

## Commands

```bash
# Web first: go build embeds web/dist.
cd web && npm ci && npm test && npm run build && cd ..

go build ./...
go vet ./...
go test -race -coverprofile=coverage.out ./...
go run ./scripts/coveragecheck -profile coverage.out -min 70
go mod verify
go run ./scripts/licensecheck -min 1
go run ./scripts/licensebundle -check

# Compose deployment check (needs Docker).
docker compose config --quiet
bash scripts/docker-smoke.sh
```

CI runs the same `go`, `web` and `docker` jobs on every PR and on `main`.

## Test-driven development

- Behavior changes start with a focused failing test, then the implementation, then the full scoped test suite.
- Bug fixes include a regression test that fails before the fix.
- CI must be green before merge. Run the checks locally when CI is not available.
- Keep Go statement coverage at or above 70%. Coverage and license checks run in CI.
- Web coverage thresholds live in `web/vite.config.ts`; run `npm test -- --coverage` in `web/`.

## Backend conventions

- Dependencies point downward: `server` → `app` → `review` / `agent` / `llm` / `gitlab` / `store`. `pkg/dag` must not import Tribunal packages.
- Migrations are append-only. Add the next numbered `internal/store/migrations/NNNN_name.sql` and never edit a released one. There are no down migrations, so document irreversible changes in `CHANGELOG.md` and `docs/operations.md`.
- Never log, return or persist plaintext secrets: tokens, webhook secrets, keys, request bodies that may contain them. Log identifiers and `errclass` categories instead of raw errors.
- Stored project secrets use `secret.Seal` / `Open` with the project and field AAD. Do not add a new call site without AAD.
- Prompt changes: bump the version comment in the prompt file and update `embeddedBundleHash` in `prompts/prompts_test.go`. The hash change must be visible in review.
- Built-in extensions (`default_extensions`) can be disabled but not deleted, are refreshed on upgrade, and must not overwrite bindings an admin changed.

## Frontend conventions

- Use CSS Modules and the tokens in `web/src/styles/tokens.css`. No component libraries, no Tailwind, no new dependencies without an Issue.
- No inline `style={}` for static styling. Inline styles are only for values computed from data, such as graph coordinates.
- Use the shared components in `web/src/components/ui.tsx`: `Button`, `ButtonLink` (never a hand-styled `<Link>`), `Toolbar`, `Segmented`, `Panel`, `Tabs`, `Dialog`, `Drawer`, badges. The same action uses the same variant on every page.
- Put page-specific styles in a page-level module (for example `projects.module.css`). Change the shared `ui.module.css` / `pages.module.css` only for shared primitives.
- Show only fields the API returns. Express state with both text and color.
- Tests query by text, role and label. When the DOM changes, update selectors and keep the assertions.
- Rebuild and commit `web/dist` with every web change. On merge conflicts in `web/dist`, take `main`'s version and rebuild; never hand-merge build output.
- Check UI changes at 1440 and 390 widths, in light and dark themes. The page itself must not scroll horizontally; tables and graphs scroll inside their own containers.

## Delivery

- One goal maps to one Issue (Why / Scope / Approach / Acceptance) and one PR. Every commit carries an `Issue: #N` trailer. Squash-merge only after CI is green, then confirm the Issue closed.
- Before merging, bring the branch up to date with the latest `main` and wait for CI to pass on the new head. A green run on an outdated base does not count: two PRs can each pass alone and still break `main` together.
- Do not create branches to host screenshots or other assets. Attach UI screenshots to the Issue through the GitHub web UI; if that is not possible, describe the checks in the PR instead.
- Add user-visible changes to the `Unreleased` or upcoming version section of `CHANGELOG.md`.
- Codex review comments are optional input. Do not block a merge waiting for a Codex re-run.
- Releases: update `CHANGELOG.md` and `docs/delivery-readiness.md`, then the maintainer tags `vX.Y.Z` and publishes the GitHub Release.
