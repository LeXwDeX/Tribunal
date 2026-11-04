# Tribunal Agent and Contributor Guide

This file is the single instruction file for every coding agent and contributor working in this repository. There is no `CLAUDE.md`; tools that look for one should read this file. It is also the project's secondary-development guide for an open-source fork.

## 1. Project

Tribunal is a multi-agent code review service for GitLab CE merge requests. It ships as a Go server with an embedded React console, SQLite storage and Docker Compose deployment. The project is MIT licensed. User-facing documentation is written in Chinese; source code, comments, commit subjects and this file are written in English unless a file states otherwise.

Goals:

- Review merge requests with a durable DAG of review blocks, evidence and adjudication.
- Keep one deployment self-contained: a single service, SQLite and per-project workspaces.
- Make review results reproducible, auditable and publishable back to GitLab.

Non-goals for the current major line:

- Multi-tenant SaaS, multi-instance coordination or a Postgres backend.
- Executing scripts from repository-provided or administrator-uploaded Skills.
- Arbitrary plugin execution outside the built-in extension surface.

Canonical documents:

- Product goals and design baseline: `ARCH.md`, `PRODUCT.md`
- Detailed design: `docs/design/` (modules, data model, HTTP API, DAG, flows, config, web console)
- Operations: `docs/operations.md`. Agent-driven deployment and integration: `docs/agent-deployment.md`
- Release notes: `CHANGELOG.md`. Verification status: `docs/delivery-readiness.md`
- Contribution and security policy: `CONTRIBUTING.md`, `SECURITY.md` when present.

## 2. Repository layout

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
| `scripts/` | Coverage, license, and log checks; load test; license bundle; Docker smoke; codebase-memory wrapper |
| `third_party/` | License and version records for bundled executables |

## 3. Commands

The full Go test suite requires Bash and a real Python 3 interpreter for the registration documentation tests. `go test -race` also requires cgo and a working C compiler. Python 3 is not a runtime dependency of the production binary or deployment; the documented manual registration steps separately require Python 3 on the host.

```bash
# Web first: go build embeds web/dist.
cd web && npm ci && npm test -- --coverage && npm run build && cd ..

go build ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
go test -race -timeout=40m -coverprofile=coverage.out ./...
go run ./scripts/coveragecheck -profile coverage.out -min 70
go mod verify
go run ./scripts/licensecheck -min 1
go run ./scripts/licensebundle -check

# Compose deployment check (needs Docker).
docker compose config --quiet
bash scripts/docker-smoke.sh
```

Run scoped checks while developing; run the full sequence before delivery when the environment supports it. CI runs the same `go`, `web` and `docker` jobs on every PR and on `main`.

GitHub Actions runs `ci.yml` on every push and pull request. The `web` and `docker` jobs use hosted runners. The `go` job uses a labeled self-hosted runner for trusted events (pushes to `main`, pull requests from branches of this repository, and a manual dispatch with `self_hosted=true`) and a hosted runner otherwise, including pull requests from forks. A repository writer may request a manual run with `gh workflow run ci.yml --ref <trusted-branch> -f self_hosted=true`; it is supplementary evidence and does not replace the push/PR checks.

Only dispatch trusted branch code to a self-hosted runner: untrusted code can persistently compromise that machine. This is an operational requirement, not a security boundary enforced automatically by GitHub or this workflow. Anyone who can modify a workflow can request a self-hosted runner; runner access controls may restrict whether it executes. Runner labels select any runner that carries them, not a specific machine. Verify runner isolation and its Docker, Docker Compose, Go, Node, Bash, Python 3 and race-enabled C compiler prerequisites before use.

### Self-hosted CI

Maintainers may run a self-hosted CI outside GitHub Actions on the development/test environment. Its rules:

- **Location.** It lives only under this project's deployment directory on the development/test environment, as the `<project-deployment-dir>/ci/` subdirectory. The Dockerfile, the run and client scripts, the README, `inbox/` and `runs/` all stay there. Image and cache-volume names use the project prefix. Do not create a sibling directory next to the project directory or anywhere else outside it.
- **Usage.** The client sends the revision under test as a git bundle to `inbox/`. Each run writes `runs/<time>-<sha>/summary.txt`, whose last line is `RESULT PASS` or `RESULT FAIL`, with per-job logs in the same directory. A full run takes several minutes; run the client in the background.
- **Coverage.** It runs the same `go`, `web` and `docker` jobs as `ci.yml`. A PASS means all three passed for that exact commit.
- **Trust and isolation.** Submit only trusted branch code. The runner stores no GitHub credentials. It must not touch other services, containers, images, volumes or directories on the same host; tear-down removes only the runner's own resources.
- **Merge gate.** A maintainer may designate a `RESULT PASS` on the self-hosted CI, for the PR head after it is brought up to date with the latest `main`, as the merge gate in place of or in addition to the GitHub Actions checks. Until a maintainer does so, the GitHub Actions checks remain the gate. The designation is recorded outside tracked files, for example in the maintainer's instructions or `docs/handoff.md`.

## 4. Open-source contribution rules

- **License and provenance.** The project is MIT licensed. By contributing, you agree to license your contribution under the same terms. Keep copyright notices and `LICENSE` intact. Do not copy code, assets or prompts from another project unless the license is compatible and the provenance is recorded. Bundled executables and third-party assets need a version and license record under `third_party/` and a regenerated license bundle.
- **Issue first.** One goal maps to one Issue with Why / Scope / Approach / Acceptance, and one PR. Small typo or dependency-bot fixes may skip the Issue only when repository policy allows it.
- **Branches and commits.** Do not push directly to `main`. Use a topic branch, keep commits focused, and include an `Issue: #N` trailer in every commit. External contributors do not need the local SpecGit checkpoint; the `Issue: #N` trailer and a normal PR are sufficient. Never use `--no-verify`; when a guard rejects a commit, fix the checkpoint or trailer instead of bypassing it.
- **Merge policy.** Squash-merge only after the current head passes the designated CI gate (GitHub Actions, or the self-hosted CI when a maintainer has designated it; see "Self-hosted CI" in section 3) and the review workflow in section 8 is complete. Bring the branch up to date with the latest `main` first; a green run on an outdated base does not count.
- **Compatibility.** Prefer additive changes. Do not break existing HTTP clients, configuration files, stored data or event consumers without an Issue that documents the migration and deprecation plan. Keep old fields readable for at least one minor release unless the change is security-critical.
- **Deprecation.** Document the replacement, the first release that warns, and the planned removal release. Do not remove a public field or endpoint silently.
- **Dependencies.** Add a dependency only when the Issue justifies it. Check the license, run `go mod verify`, regenerate the license bundle, and keep `web` free of unjustified npm packages.
- **Secrets.** Never commit tokens, keys, `.env` files, webhook secrets or request bodies that may contain them. Log identifiers and `errclass` categories, not raw errors. Report a vulnerability through the private security advisory process, not a public Issue.
- **Generated artifacts.** `web/dist` is committed because the Go binary embeds it; rebuild it with every web change. Never hand-merge build output. Local environment files, editor state and agent scratch files stay untracked.
- **Documentation.** User-visible documentation is Chinese. Update `CHANGELOG.md` for user-visible changes, `docs/operations.md` for operational behavior, and `docs/delivery-readiness.md` for verification status. Documentation-only PRs follow the reduced review chain in section 8.
- **Release authority.** Maintainers tag releases and publish GitHub Releases. Agents and contributors do not create tags or Releases unless explicitly authorized. Use semantic versioning; breaking changes require a major version and a written migration path.

## 5. Architecture and backend conventions

- Dependencies point downward: `server` → `app` → `review` / `agent` / `llm` / `gitlab` / `store`. `pkg/dag` must not import Tribunal packages.
- Keep the DAG engine scenario-agnostic. Product-specific behavior belongs in `internal/review` or `internal/app`.
- Migrations are append-only. Add the next numbered `internal/store/migrations/NNNN_name.sql` and never edit a released one. There are no down migrations, so document irreversible changes in `CHANGELOG.md` and `docs/operations.md`.
- Never log, return or persist plaintext secrets: tokens, webhook secrets, keys, request bodies that may contain them. Log identifiers and `errclass` categories instead of raw errors.
- Stored project secrets use `secret.Seal` / `Open` with the project and field AAD. Do not add a new call site without AAD.
- Prompt changes: bump the version comment in the prompt file and update `embeddedBundleHash` in `prompts/prompts_test.go`. The hash change must be visible in review. Changes to task layout or coverage semantics also bump `prompts.TaskRenderVersion`.
- Built-in extensions (`default_extensions`) are platform-managed: they are enabled on every start, cannot be disabled or deleted through the API, and are refreshed on upgrade without overwriting bindings an administrator changed. Custom extension creation is closed; existing custom entries keep their management operations.
- Configuration is loaded once at startup unless a feature explicitly reloads it. Database-backed extension, binding and account changes generally require a restart. Document the exact timing when adding a new setting.
- Preserve invariants around SQLite transactions, workspace locks, publication generations and project deletion. When a change touches one of those paths, add a focused concurrency or failure test.
- API responses must not leak internal paths, raw upstream bodies, secrets or existence information the caller is not authorized to see.

## 6. Frontend conventions

- Use CSS Modules and the tokens in `web/src/styles/tokens.css`. No component libraries, no Tailwind, no new dependencies without an Issue.
- No inline `style={}` for static styling. Inline styles are only for values computed from data, such as graph coordinates.
- Use the shared components in `web/src/components/ui.tsx`: `Button`, `ButtonLink` (never a hand-styled `<Link>`), `Toolbar`, `Segmented`, `Panel`, `Tabs`, `Dialog`, `Drawer`, badges. The same action uses the same variant on every page.
- Put page-specific styles in a page-level module (for example `projects.module.css`). Change the shared `ui.module.css` / `pages.module.css` only for shared primitives.
- Show only fields the API returns. Express state with both text and color.
- Tests query by text, role and label. When the DOM changes, update selectors and keep the assertions. Prefer accessible names over CSS selectors.
- Rebuild and commit `web/dist` with every web change. On merge conflicts in `web/dist`, take `main`'s version and rebuild; never hand-merge build output.
- Check UI changes at 1440 and 390 widths, in light and dark themes. The page itself must not scroll horizontally; tables and graphs scroll inside their own containers.
- Do not hide a destructive or privileged action only in CSS; remove it from the render tree and enforce the same rule on the server.

## 7. Testing requirements

- **TDD.** Behavior changes start with a focused failing test, then the implementation, then the scoped suite. Bug fixes include a regression test that fails before the fix. A test that only changes an assertion to match new behavior is not a regression test.
- **Layers.** Cover the lowest useful layer first: pure functions and package tests, then HTTP wiring and authorization, then component tests, then Docker smoke. Add an end-to-end test only when the behavior cannot be proven below it.
- **Determinism.** Tests must not depend on real network access, real credentials, wall-clock races, fixed ports or shared mutable state. Use fakes, temporary directories, fake clocks and injected clients. A test that binds a fixed port or relies on a real external service belongs in an explicitly gated acceptance suite, not the default unit suite.
- **Concurrency.** Run race-enabled tests for code that touches goroutines, stores, workspaces, jobs or publication. Use `-race` and the repository timeout; do not weaken the timeout to hide a hang.
- **Coverage.** Keep Go statement coverage at or above 70%; web thresholds live in `web/vite.config.ts`; run `npm test -- --coverage` in `web/`. Coverage is a floor, not a target: do not add assertion-free tests or duplicate paths only to raise the number.
- **Flakiness.** If a check fails intermittently, rerun once, record the failing test and the run, and open an Issue if it recurs. Do not mark a flaky test as passed, skip it silently, or weaken an assertion. A green rerun is evidence only when the same commit is identified.
- **Isolation from environments.** Do not hardcode internal hosts, ports, tokens, project IDs or deployment paths in tests or documentation examples. Use environment variables, fixtures or placeholders. Real-deployment acceptance is separate from CI and must be labeled as such.
- **GitLab integration tests.** Opt-in tests that call a real GitLab Markdown or labels API read `TRIBUNAL_TEST_GITLAB_HOST` and `TRIBUNAL_TEST_GITLAB_PROJECT`. The GL issue-reference test reads `TRIBUNAL_TEST_GITLAB_HOST`, `TRIBUNAL_TEST_GITLAB_ISSUE_PROJECT` and `TRIBUNAL_TEST_GITLAB_ISSUE_IID`; it does not need `TRIBUNAL_TEST_GITLAB_PROJECT`. All require `TRIBUNAL_TEST_GITLAB_MARKDOWN=1`. When a required variable is unset, the test skips with the missing variable name. A skip is not a PASS: delivery reports must still list the unrun real-environment check as BLOCKED. Do not embed the host or project path in the test.
- **Real environments and credentials.** Real-deployment, Docker, real GitLab and real LLM API tests require the user to provide the test server, Docker daemon, GitLab project and LLM API key/endpoint. Do not assume they exist, do not reuse production credentials, and do not silently replace a missing environment with mocks. If the environment or credential is missing, report the check as BLOCKED, name what is missing, and continue only with local or CI checks. Ask the user for the environment when the Issue's acceptance criteria require it.
- **Documentation checks.** For documentation changes, verify links and anchors, command examples and factual claims. A docs PR still needs an independent review and an evidence check; it does not need unrelated runtime tests.
- **No test-only backdoors.** Do not add production branches that exist only to make a test pass. Extract a seam or inject a dependency instead.
- **Test names and failures.** Name tests after the behavior, not the implementation. Failure messages must identify the input and the expected result without printing secrets.

## 8. Development review workflow

Every behavior change and every user-visible document change goes through these stages before merge, unless the reduced chain below applies. CI green is necessary but not sufficient; a PR that skips a stage is not ready.

1. **Independent review** — a `review` subagent reads the real diff, callers, tests, and evidence. Give it the Issue, the diff range, and the evidence; do not give it the author's conclusion.
2. **Three-way review** — run the development review subagents `reasoner`, `verify`, and `review` in parallel on the same diff. `reasoner` stress-tests logic, boundaries, races, and failure paths; `verify` reruns the scoped checks and reports PASS/FAIL/BLOCKED; `review` checks requirements, compatibility, and the evidence chain. These are development subagents, distinct from the product's review blocks with the same names.
3. **Adversarial review** — run several independent reviewers with distinct lenses (for example security/secrets, concurrency/correctness, accessibility/regression, documentation/claims). The goal is to find collective blind spots, not to repeat the first review.
4. **Adjudication and evidence check** — the main agent deduplicates findings, classifies P1/P2/P3, and re-checks every material claim against source or a reproducible command. P1 is blocking; P2/P3 are non-blocking unless explicitly marked blocking with a reason. A subagent finding is a lead, not a fact; do not forward it without this check.
5. **Fix and re-review** — fix confirmed findings with a regression test that fails before the fix. A blocking finding needs a new independent review of the fix; never self-approve a blocking change.
6. **Structured report** — publish a report on the Issue or PR with an overall status, one unit per finding, the author's reply, and the reviewer's reply to that reply. Mark each unit resolved, accepted, or blocking. Disclose unverified items and environment blockers; never present mock, partial, or CI-only results as real evidence.

A documentation-only PR may use a reduced chain: independent review plus evidence check, and it still publishes a short report (on the Issue, the PR, or both). A documentation finding is verified with links, formatting, or fact checks instead of a regression test; a blocking documentation finding still needs a new independent review of the fix. It still must not fabricate verification.

If a stage cannot run because a credential, environment, an external service, or the required agent capability is missing, name the missing stage and why. Do not replace it with CI alone.

When the named subagents are unavailable, the same checks may be performed manually
by separate reviewers: one independent code review, one logic and boundary pass,
one scoped test run, and one adversarial pass. Record who performed each check and
the evidence; CI alone is not a substitute.

## 9. Acceptance and release

- **Issue acceptance.** Every Issue has testable acceptance criteria. A PR description maps each criterion to a check, a command result, or an explicitly accepted gap.
- **PR gate.** The PR must be based on the latest `main`, pass the current head's `go`/`web`/`docker` checks on the designated CI gate (section 3), complete the applicable review workflow, and include the `Issue: #N` trailer. Do not merge on an outdated base.
- **Evidence.** Report the exact command, scope, and result. Separate local, CI and real-deployment evidence. Never present a mock, a partial run, or a CI-only result as a real deployment validation. If a check could not run, say why.
- **Real-deployment acceptance.** When an Issue requires it, use an isolated deployment with a backup, verify the expected migration set and health endpoints, exercise a real review and the changed feature, and confirm rollback is possible. Do not write host names, ports, tokens or internal paths into public documents; describe them as environment-specific.
- **Release.** Maintainers update `CHANGELOG.md` and `docs/delivery-readiness.md`, then tag `vX.Y.Z` and publish the GitHub Release. Agents do not tag or publish unless explicitly authorized. A release note must distinguish verified scope from remaining gaps.
- **Rollback.** There are no down migrations. A rollback restores the pre-upgrade snapshot and a matching binary; replacing only the image is not supported. Document the backup boundary before an irreversible migration.
- **Post-merge.** Read back the PR and Issue state, confirm the Issue is closed, update `docs/handoff.md` in the primary checkout when the state changes, and remove temporary worktrees.

## 10. Operations and handoff

- The image does not rewrite a host-mounted configuration file. Upgrades must compare the mounted file with the shipped example and decide which values to keep.
- Back up the database, any `-wal`/`-shm` files, all workspaces, configuration, image digest and encryption key before an irreversible upgrade. A running-file copy is not a consistent backup.
- Do not run two containers against the same SQLite data directory. Use the lock and health endpoints as the first checks.
- `docs/handoff.md` is a local, untracked handoff note for the next agent on the same machine. It lives only in the primary checkout, not in a temporary worktree, because ignored files are per worktree and are lost when the worktree is removed. It is ignored by `.gitignore`, never committed and never updated through a PR. It states the current state, the next actions, known gaps and environment-specific notes, but never secrets. When it is missing, rebuild the state from the open Issues, recent PRs, `CHANGELOG.md` and `docs/delivery-readiness.md`, and ask the user for the environment-specific notes, which no tracked source holds.
- Public, durable state lives in Issues, PRs, `CHANGELOG.md` and `docs/delivery-readiness.md`. Keep internal host names, credentials and private deployment details out of every tracked file. The repository is public.

### Agent test account

- On the development/test environment, an agent creates and uses its own dedicated test account to sign in and monitor deployments, jobs, reviews and publication results. This is a Tribunal console account and, when acceptance requires it, a dedicated identity on the test GitLab.
- Never reuse a human maintainer's account or any production account. Grant only the permissions monitoring needs.
- Keep its credentials only on that environment's host side, for example in the environment's own untracked `.env` file or the environment-local, untracked handoff note. Never put them in a tracked file, a PR, an Issue or a log.
- Do not create such accounts in production.

## 11. Agent workflow

- Read this file, then `docs/handoff.md` in the primary checkout if it exists, and the open Issues before starting work. Do not redo completed Issues.
- Prefer a subagent or a bounded task for long-running monitoring. Do not use `sleep` or a foreground `--watch` to wait for CI; run the self-hosted CI client in the background and read its `summary.txt`. After starting a PR, continue independent work or report progress and end the turn only when a background callback is configured and verified to wake the session. If no such callback exists or it is unreliable, use a monitored subagent with an explicit timeout, or report the limitation instead of silently waiting.
- Use SpecGit as required by the repository. Do not commit with `--no-verify`. If a checkpoint belongs to another branch, use a separate worktree and initialize it from the main declaration instead of overwriting the checkpoint. External contributors without SpecGit use a normal topic branch and PR with the `Issue: #N` trailer; the local checkpoint is a maintainer convenience, not a contribution requirement.
- Respect permission prompts. Do not bypass a denied command, a protected branch or a credential boundary. Stop and ask when the next action needs human authorization or a secret.
- A subagent's finding is a lead, not a fact. Verify it against source or a reproducible command before reporting or acting on it.
- Keep the final report short: what changed, what was verified, what was not verified, and the remaining risk.

The SpecGit section below describes the maintainer's local checkpoint setup. External
contributors can ignore it and use a normal topic branch and PR.

<!-- specgit:project:v2:start -->
## SpecGit 2

Read project-root `.agents/skills/specgit-native/SKILL.md` before work; if missing, initialize with the shared `specgit init` CLI or restore an existing integration with `specgit setup` ([installation](https://github.com/LeXwDeX/SpecGit/blob/main/docs/agent-install.md)).
<!-- specgit:project:v2:end -->
