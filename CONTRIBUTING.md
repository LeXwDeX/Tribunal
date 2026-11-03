# Contributing

Read [`AGENTS.md`](AGENTS.md) before changing code or user-visible documentation.
It is the single source for the project layout, commands, coding conventions,
test requirements, review workflow and release policy.

## Quick checklist

- Open an Issue with Why / Scope / Approach / Acceptance before a behavior change.
- Use a topic branch and put `Issue: #N` in every commit trailer.
- Keep changes additive where possible; document migrations and deprecations.
- Run the scoped checks from `AGENTS.md` §3, then the current-head CI.
- Complete the review workflow in `AGENTS.md` §8 and publish a short report; a green CI alone is not enough.
- Do not commit secrets, `.env` files or local agent state.
- Report security issues privately through [`SECURITY.md`](SECURITY.md).

By contributing, you agree to license your contribution under the project's MIT
license.
