# code-review skill provenance

- Upstream: https://github.com/mattpocock/skills
- Listing: https://www.skills.sh/mattpocock/skills/code-review
- Source file: `skills/engineering/code-review/SKILL.md`
- Upstream commit: `3da8c01116264675eeefbd6e9d59c734de9095cc` (2026-10-07)
- License: MIT, Copyright (c) 2026 Matt Pocock. The full text is in `LICENSE`.

## Tribunal modifications

Tribunal ships an adapted, non-interactive version for its read-only merge
request review agents:

- The fixed point is the merge request base/target SHA supplied by the
  platform, instead of a ref chosen interactively.
- The spec is the merge request title, description and the issues it
  references, as supplied by the platform; no issue tracker lookup or setup
  command is required.
- The Standards and Spec axes map to Tribunal's existing `review.standards`,
  `review.intent` and `code-review` blocks instead of parallel helper agents.
- Findings are reported through the block's `submit_result` contract.
- Kept unchanged: the two-axis model, the Fowler smell baseline, "the
  repository overrides" and "always a judgement call".

The upstream `agents/openai.yaml` file is not included.

Tribunal's changes are distributed under the same MIT terms.
