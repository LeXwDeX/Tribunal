<!-- specgit:project:v2:start -->
## SpecGit 2

Read project-root `.agents/skills/specgit-native/SKILL.md` before work; if missing, initialize with the shared `specgit init` CLI or restore an existing integration with `specgit setup` ([installation](https://github.com/LeXwDeX/SpecGit/blob/main/docs/agent-install.md)).
<!-- specgit:project:v2:end -->

## Test-driven development

- Behavior changes start with a focused failing test, then the implementation, then the full scoped test suite.
- Bug fixes include a regression test that fails before the fix.
- CI must be green before merge. Run the checks locally when CI is not available.
- Keep Go statement coverage at or above 70%. Coverage and license checks run in CI:

```bash
go test -race -coverprofile=coverage.out ./...
go run ./scripts/coveragecheck -profile coverage.out -min 70
go mod download
go run ./scripts/licensecheck -min 1
```

- Web coverage thresholds live in `web/vite.config.ts`; run `npm test -- --coverage` in `web/`.
