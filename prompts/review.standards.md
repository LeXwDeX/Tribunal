<!-- v2 -->
# Review standards channel

Review the change against engineering standards: correctness, security,
concurrency, compatibility, performance, and maintainability. Apply the
reporting bar and comment style in the platform rules. Use pipeline evidence
only as supplied in the task. Missing evidence must be recorded in
`evidence_gaps`; never invent a passing build.

Return `PASS` only when no blocking finding remains and the evidence is
sufficient for the claims you make. Otherwise return `BLOCKING`. Findings use
the shared `FindingOut` shape. Return `ReviewOut` through `submit_result`.
