# Tribunal

<!-- impeccable:product-schema 1 -->

This record summarizes the existing ARCH.md, docs/design, and local prototype.
It is not evidence that a feature is implemented or deployed. No new product
interview was completed during autonomous execution; inferred context is marked.

## Platform

web

## Stack

The console uses Vite, React, and TypeScript. Its static build is embedded
in the Go service. SQLite is the first supported storage backend. See
[the console specification](docs/design/07-web-console.md).

## Users

Platform administrators register GitLab projects, inspect review runs, maintain
project context and extensions, and diagnose failures. Viewers inspect the same
review evidence without changing platform state.

## Product Purpose

Review GitLab CE merge requests using API-key-funded models. A main agent plans
review work, independent reviewers propose findings, challengers try to disprove
them, and deterministic checks constrain the final published result.

## Operating Context

GitLab remains the source of merge requests, repository contents, CI evidence,
and review comments. The console provides a view of workflow state and evidence.
Inferred from the internal-tool brief: users usually inspect dense information
on a desktop, but must also be able to operate the console at 390px width.

## Capabilities and Constraints

- The service reviews code without running repository tests, builds, or scripts.
- It does not push changes or merge merge requests.
- Repository context and configuration come from the target branch. Generated
  context, indexes, and worktrees remain inside the platform.
- GitLab OAuth sessions support admin and viewer roles. Browser writes require
  CSRF protection; CLI access uses API tokens.
- An execution failure or incomplete verification must not appear as a PASS.
- Provider secrets, project tokens, and webhook secrets must not appear in UI
  responses or logs. Newly issued secrets are displayed only when required.
- Real GitLab, LLM-provider, and external-index validation remain distinct from
  local fixture tests.

## Brand Commitments

Use the user-approved Tribunal name and the existing Chinese console labels. The
local prototype still defines the visual reference; this rename is branding, not
a redesign of the console or review flow.

## Evidence on Hand

- [Architecture and scope](ARCH.md).
- [HTTP API](docs/design/03-http-api.md).
- [Web console](docs/design/07-web-console.md).
- Local, gitignored `prototype/*.dc.html` contains the six reference screens.
  Their example metrics and project names are illustrative, not live data.

## Product Principles

- Show the finding, its evidence, challenge, and disposition together.
- Make failed, cancelled, incomplete, and stale results visibly distinct.
- Keep permissions and secrets explicit at every boundary.
- Use actual persisted state; never substitute sample success for missing data.

## Accessibility & Inclusion

The specification requires usable narrow-screen pages and contained table
scrolling. Engineering defaults add keyboard operation, visible focus, semantic
labels, adequate contrast, and reduced-motion support; these are implementation
defaults, not a newly confirmed product-specific accessibility certification.
