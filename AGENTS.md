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

Run scoped checks while developing; run the full sequence before delivery when the environment supports it.
## 4. Open-source contribution rules

- **License and provenance.** The project is MIT licensed. By contributing, you agree to license your contribution under the same terms. Keep copyright notices and `LICENSE` intact. Do not copy code, assets or prompts from another project unless the license is compatible and the provenance is recorded. Bundled executables and third-party assets need a version and license record under `third_party/` and a regenerated license bundle.
- **Issue first.** One goal maps to one Issue with Why / Scope / Approach / Acceptance, and one PR. Small typo or dependency-bot fixes may skip the Issue only when repository policy allows it.
- **Branches and commits.** Do not push directly to `main`. Use a topic branch, keep commits focused, and include an `Issue: #N` trailer in every commit. Never use `--no-verify`; when a guard rejects a commit, fix the checkpoint or trailer instead of bypassing it.
- **Merge policy.** Squash-merge only, after the merge gate (section 9).
- **Compatibility.** Prefer additive changes. Do not break existing HTTP clients, configuration files, stored data or event consumers without an Issue that documents the migration and deprecation plan. Keep old fields readable for at least one minor release unless the change is security-critical.
- **Deprecation.** Document the replacement, the first release that warns, and the planned removal release. Do not remove a public field or endpoint silently.
- **Dependencies.** Add a dependency only when the Issue justifies it. Check the license, run `go mod verify`, regenerate the license bundle, and keep `web` free of unjustified npm packages.
- **Secrets.** Never commit tokens, keys, `.env` files, webhook secrets or request bodies that may contain them, and never put credentials in a PR, an Issue or a log. Report a vulnerability through the private security advisory process, not a public Issue.
- **Generated artifacts.** `web/dist` is committed because the Go binary embeds it (rebuild rules: section 6). Local environment files, editor state and agent scratch files stay untracked.
- **Documentation.** User-visible documentation is Chinese. Update `CHANGELOG.md` for user-visible changes, `docs/operations.md` for operational behavior, and `docs/delivery-readiness.md` for verification status. Documentation-only PRs follow the reduced review chain in section 8.
- **Release authority.** Tags and GitHub Releases follow stage 4 of the development and release workflow (section 9). Use semantic versioning; breaking changes require a major version and a written migration path.

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

- Use CSS Modules and the tokens in `web/src/styles/tokens.css`. No component libraries, no Tailwind.
- No inline `style={}` for static styling. Inline styles are only for values computed from data, such as graph coordinates.
- Use the shared components in `web/src/components/ui.tsx`: `Button`, `ButtonLink` (never a hand-styled `<Link>`), `Toolbar`, `Segmented`, `Panel`, `Tabs`, `Dialog`, `Drawer`, badges. The same action uses the same variant on every page.
- Put page-specific styles in a page-level module (for example `projects.module.css`). Change the shared `ui.module.css` / `pages.module.css` only for shared primitives.
- Show only fields the API returns. Express state with both text and color.
- Tests query by text, role and label. When the DOM changes, update selectors and keep the assertions. Prefer accessible names over CSS selectors.
- Rebuild and commit `web/dist` with every web change. On merge conflicts in `web/dist`, take `main`'s version and rebuild; never hand-merge build output.
- Check UI changes at 1440 and 390 widths, in light and dark themes. The page itself must not scroll horizontally; tables and graphs scroll inside their own containers.
- Do not hide a destructive or privileged action only in CSS; remove it from the render tree and enforce the same rule on the server.
- Use the shared form-control classes `ui.input` / `ui.select` / `ui.textarea` (or the global `input` / `select` / `textarea` classes). Never leave a form control on the browser default appearance; `select` uses `appearance: none` plus the shared chevron.
- Prefer the form primitives in `web/src/components/ui.tsx` — `Select`, `Checkbox`, `DatePicker`, `NumberField`, `SearchField`, `Textarea`, `Tooltip`, `Disclosure` — on new pages and forms. Do not hand-roll native controls or use DOM `title` for tooltips.
- Explanation text placement: `Field` hint goes below the control and is linked with `aria-describedby` (`<htmlFor>-hint`) automatically only for a single control child; multi-control fields (for example select + input) must set `aria-describedby` manually. `PageHeader` description goes below the title; `Panel` `note` / `TableNote` go after the content (`.panelNote` for non-flush panels, `.tableNote` when `flush`); `Toolbar` `status` goes after the `role="toolbar"` element, never inside its actions; `ActionHint` goes below the button group.
- Keep `Toolbar` `status` mounted when announcements must be reliable: pass a string, and note that an empty string still renders the live region; `undefined` unmounts it.
- Keep `Panel` `aside` for short counts, status, source labels, links and actions only; do not put long explanations there. Warnings and errors keep `role="alert"` / `role="status"`; do not downgrade them to plain hints.

## 7. Testing requirements

- **TDD.** Behavior changes start with a focused failing test, then the implementation, then the scoped suite. Bug fixes include a regression test that fails before the fix. A test that only changes an assertion to match new behavior is not a regression test.
- **Layers.** Cover the lowest useful layer first: pure functions and package tests, then HTTP wiring and authorization, then component tests, then Docker smoke. Add an end-to-end test only when the behavior cannot be proven below it.
- **Determinism.** Tests must not depend on real network access, real credentials, wall-clock races, fixed ports or shared mutable state. Use fakes, temporary directories, fake clocks and injected clients. A test that binds a fixed port or relies on a real external service belongs in an explicitly gated acceptance suite, not the default unit suite.
- **Concurrency.** Run race-enabled tests for code that touches goroutines, stores, workspaces, jobs or publication. Use `-race` and the repository timeout; do not weaken the timeout to hide a hang.
- **Coverage.** Keep Go statement coverage at or above 70%; web thresholds live in `web/vite.config.ts`; run `npm test -- --coverage` in `web/`. Coverage is a floor, not a target: do not add assertion-free tests or duplicate paths only to raise the number.
- **Flakiness.** If a check fails intermittently, rerun once, record the failing test and the run, and open an Issue if it recurs. Do not mark a flaky test as passed, skip it silently, or weaken an assertion. A green rerun is evidence only when the same commit is identified.
- **Isolation from environments.** Do not hardcode internal hosts, ports, tokens, project IDs or deployment paths in tests or documentation examples. Use environment variables, fixtures or placeholders.
- **GitLab integration tests.** Opt-in tests that call a real GitLab Markdown or labels API read `TRIBUNAL_TEST_GITLAB_HOST` and `TRIBUNAL_TEST_GITLAB_PROJECT`. The GL issue-reference test reads `TRIBUNAL_TEST_GITLAB_HOST`, `TRIBUNAL_TEST_GITLAB_ISSUE_PROJECT` and `TRIBUNAL_TEST_GITLAB_ISSUE_IID`; it does not need `TRIBUNAL_TEST_GITLAB_PROJECT`. All require `TRIBUNAL_TEST_GITLAB_MARKDOWN=1`. When a required variable is unset, the test skips with the missing variable name. A skip is not a PASS: delivery reports must still list the unrun real-environment check as BLOCKED.
- **Real environments and credentials.** Real-deployment, Docker, real GitLab and real LLM API tests require the user to provide the test server, Docker daemon, GitLab project and LLM API key/endpoint. Do not assume they exist, do not reuse production credentials, and do not silently replace a missing environment with mocks. If the environment or credential is missing, report the check as BLOCKED, name what is missing, and continue only with local or CI checks. Ask the user for the environment when the Issue's acceptance criteria require it.
- **Documentation checks.** For documentation changes, verify links and anchors, command examples and factual claims.
- **No test-only backdoors.** Do not add production branches that exist only to make a test pass. Extract a seam or inject a dependency instead.
- **Test names and failures.** Name tests after the behavior, not the implementation. Failure messages must identify the input and the expected result without printing secrets.

## 8. Development review workflow

Every behavior change and every user-visible document change goes through these stages before merge, unless the reduced chain below applies. A PR that skips a stage is not ready.

1. **Independent review** — a `review` subagent reads the real diff, callers, tests, and evidence. Give it the Issue, the diff range, and the evidence; do not give it the author's conclusion.
2. **Three-way review** — run the development review subagents `reasoner`, `verify`, and `review` in parallel on the same diff. `reasoner` stress-tests logic, boundaries, races, and failure paths; `verify` reruns the scoped checks and reports PASS/FAIL/BLOCKED; `review` checks requirements, compatibility, and the evidence chain. These are development subagents, distinct from the product's review blocks with the same names.
3. **Adversarial review** — run several independent reviewers with distinct lenses (for example security/secrets, concurrency/correctness, accessibility/regression, documentation/claims). The goal is to find collective blind spots, not to repeat the first review.
4. **Adjudication and evidence check** — the main agent deduplicates findings, classifies P1/P2/P3, and re-checks every material claim against source or a reproducible command. P1 is blocking; P2/P3 are non-blocking unless explicitly marked blocking with a reason.
5. **Fix and re-review** — fix confirmed findings with a regression test that fails before the fix. A blocking finding needs a new independent review of the fix; never self-approve a blocking change.
6. **Structured report** — publish a report on the Issue or PR with an overall status, one unit per finding, the author's reply, and the reviewer's reply to that reply. Mark each unit resolved, accepted, or blocking. Disclose unverified items and environment blockers.

A documentation-only PR may use a reduced chain: independent review plus evidence check, and it still publishes a short report (on the Issue, the PR, or both). A documentation finding is verified with links, formatting, or fact checks instead of a regression test; a blocking documentation finding still needs a new independent review of the fix. It still must not fabricate verification.

If a stage cannot run because a credential, environment, an external service, or the required agent capability is missing, name the missing stage and why. Do not replace it with CI alone.

When the named subagents are unavailable, the same checks may be performed manually
by separate reviewers: one independent code review, one logic and boundary pass,
one scoped test run, and one adversarial pass. Record who performed each check and
the evidence.

## 9. Development and release workflow

The general release workflow (self-hosted CI → GitHub Runner → human review → tag + GitHub Release) is defined in the maintainer's global agent instructions; this section records only project-specific requirements.

1. **Development (self-hosted CI).** PRs merge through the merge gate below. Exit: all known issues for the release are resolved, `CHANGELOG.md` and `docs/delivery-readiness.md` are updated for the release, and a release candidate commit on `main` is chosen. The candidate is frozen once chosen; a new commit on `main` makes a new candidate that restarts stage 2.
2. **GitHub Runner.** Run all `ci.yml` jobs (`go`, `web`, `docker`) for the release candidate commit on `main` (the automatic run from its push to `main`, or a manual dispatch on that commit); all three must pass for that exact commit. A failure sends the work back to stage 1; the fix goes through a normal PR and stage 2 reruns on the new candidate.
3. **Human review on production.** Review the same candidate (same commit and image). Each production deployment needs explicit maintainer approval and follows the Rollback rule below and the backup rule in section 10. A problem found sends the work back to stage 1.
4. **Tag and release.** A maintainer, or an agent explicitly authorized for that release, tags `vX.Y.Z` on the exact commit that passed stages 2 and 3 and publishes the GitHub Release. The release note distinguishes verified scope from remaining gaps.

### Self-hosted CI (stage 1 merge gate)

The self-hosted CI is a Docker-based runner on the development/test environment, outside GitHub Actions. It is the merge gate for every PR. Its rules:

- **Location.** The `ci/` subdirectory of the project's deployment directory holds the Dockerfile, the run and client scripts, the README, `inbox/` and `runs/`. Image and cache-volume names use the project prefix.
- **Usage.** The client sends the revision under test as a git bundle to `inbox/`. Each run writes `runs/<time>-<sha>/summary.txt`, whose last line is `RESULT PASS` or `RESULT FAIL`, with per-job logs in the same directory. A full run takes several minutes.
- **Coverage.** It runs the same steps as the `go`, `web` and `docker` jobs in `ci.yml` and must be kept in sync with that file. A PASS means all three passed for that exact commit.
- **Trust and isolation.** Pull requests from forks are not submitted until a maintainer has reviewed the code and judged it trusted. The runner stores no GitHub credentials.
- **Merge gate.** A PR merges only when the self-hosted CI reports `RESULT PASS` for the PR head after it is brought up to date with the latest `main` (a green run on an outdated base does not count) and the applicable review workflow (section 8) is complete. Development does not wait for GitHub Actions results. The PR states the tested commit SHA and the `RESULT PASS` summary line.

### GitHub Runner (stage 2)

GitHub Actions workflows live in `.github/workflows/`; triggers, job layout, runner selection and manual dispatch inputs are defined only there. A self-hosted runner used by the `go` job belongs to GitHub Actions; it is not the self-hosted CI.

Only dispatch trusted branch code to a self-hosted runner: untrusted code can persistently compromise that machine. Pull requests from branches of this repository run the `go` job on it automatically, so push only trusted code to branches of this repository; untrusted contributions come from forks. This is an operational requirement, not a security boundary enforced automatically by GitHub or `ci.yml`. Anyone who can modify a workflow file can request a self-hosted runner; runner access controls may restrict whether it executes. Runner labels select any runner that carries them, not a specific machine. Verify runner isolation and its Docker, Docker Compose, Go, Node, Bash, Python 3 and race-enabled C compiler prerequisites before use.

### Acceptance rules

- **Issue acceptance.** Every Issue has testable acceptance criteria. A PR description maps each criterion to a check, a command result, or an explicitly accepted gap.
- **Evidence.** Report the exact command, scope, and result. Separate local, CI and real-deployment evidence. Never present a mock, a partial run, or a CI-only result as a real deployment validation.
- **Real-deployment acceptance.** When an Issue requires it, use an isolated deployment with a backup, verify the expected migration set and health endpoints, exercise a real review and the changed feature, and confirm rollback is possible. This is Issue-level acceptance on the development/test environment; it is separate from the production human review in stage 3 and neither replaces the other.
- **Rollback.** With no down migrations (section 5), a rollback restores the pre-upgrade snapshot and a matching binary; replacing only the image is not supported.
- **Post-merge.** Read back the PR and Issue state, confirm the Issue is closed, update `docs/handoff.md` in the primary checkout when the state changes, and remove temporary worktrees.

## 10. Operations and handoff

- The image does not rewrite a host-mounted configuration file. Upgrades must compare the mounted file with the shipped example and decide which values to keep.
- Back up the database, any `-wal`/`-shm` files, all workspaces, configuration, image digest and encryption key before an irreversible upgrade. A running-file copy is not a consistent backup. Document the backup boundary before an irreversible migration.
- Do not run two containers against the same SQLite data directory. Use the lock and health endpoints as the first checks.
- `docs/handoff.md` is a local, untracked handoff note for the next agent on the same machine. It lives only in the primary checkout, not in a temporary worktree, because ignored files are per worktree and are lost when the worktree is removed. It is ignored by `.gitignore`, never committed and never updated through a PR. It states the current state, the next actions, known gaps and environment-specific notes, but never secrets. When it is missing, rebuild the state from the open Issues, recent PRs, `CHANGELOG.md` and `docs/delivery-readiness.md`, and ask the user for the environment-specific notes, which no tracked source holds.
- Public, durable state lives in Issues, PRs, `CHANGELOG.md` and `docs/delivery-readiness.md`. The repository is public: keep internal host names, ports, internal paths and private deployment details out of every tracked file, PR and Issue, and describe them as environment-specific. Environment details (hosts, deployment directories, how to invoke the self-hosted CI client) and host operating rules live in the maintainer's global agent instructions and untracked local files such as `docs/handoff.md`.

### Agent test account

- The console has no local accounts; browser login is GitLab OAuth only (`/auth/login`, `/auth/callback`). On the development/test environment, the agent test account is therefore a dedicated user on the test GitLab that signs in to the test instance through OAuth to monitor deployments, jobs, reviews and publication results. Ask a GitLab administrator to create it when the agent lacks the privilege.
- The test instance uses its own GitLab OAuth application: callback URI is the test instance's `/auth/callback`, scope `read_user`. Never reuse the production OAuth application.
- The static bootstrap bearer token is for platform administration only (for example registering projects and setting project grants through the admin API). It is user ID 0 and never holds a project grant, so project-level APIs return 404 for it.
- Project-level APIs (for example rotating a project token or manually triggering a review) need an API token created by a signed-in user, and that user needs the project grant they require (manage for those two). API tokens can only be created from a browser session, through the console or `POST /api/v1/api-tokens`.
- Grant the test account only the permissions monitoring needs: a read grant by default, and manage only when the task needs token rotation or a manual review trigger. Never reuse a human maintainer's account (except as below) or any production account.
- Keep the details of the fixed test repository used for acceptance, and the test tokens, only in the development/test environment, in an untracked env file with mode 0600, and refer to them by variable name; never print their values. `docs/handoff.md` may name the account and where its credentials live.
- Exception to the human-account rule: if no dedicated GitLab user can be obtained, a maintainer's own GitLab account with the fixed test repository may stand in temporarily, with the maintainer's consent. List it as a known gap in the delivery report and replace it once a dedicated user exists.
- Do not create such accounts in production.

## 11. Agent workflow

- Read this file, then `docs/handoff.md` in the primary checkout if it exists, and the open Issues before starting work. Do not redo completed Issues.
- Prefer a subagent or a bounded task for long-running monitoring. Do not use `sleep` or a foreground `--watch` to wait for CI; run the self-hosted CI client in the background and read its `summary.txt`. After starting a PR, continue independent work or report progress and end the turn only when a background callback is configured and verified to wake the session. If no such callback exists or it is unreliable, use a monitored subagent with an explicit timeout, or report the limitation instead of silently waiting.
- Use SpecGit as required by the repository. If a checkpoint belongs to another branch, use a separate worktree and initialize it from the main declaration instead of overwriting the checkpoint.
- Respect permission prompts. Do not bypass a denied command, a protected branch or a credential boundary. Stop and ask when the next action needs human authorization or a secret.
- A subagent's finding is a lead, not a fact. Verify it against source or a reproducible command before reporting or acting on it.
- Keep the final report short: what changed, what was verified, what was not verified, and the remaining risk.

The SpecGit section below describes the maintainer's local checkpoint setup. External
contributors can ignore it and use a normal topic branch and PR.

<!-- specgit:project:v2:start -->
## SpecGit 2

Read project-root `.agents/skills/specgit-native/SKILL.md` before work; if missing, initialize with the shared `specgit init` CLI or restore an existing integration with `specgit setup` ([installation](https://github.com/LeXwDeX/SpecGit/blob/main/docs/agent-install.md)).
<!-- specgit:project:v2:end -->
