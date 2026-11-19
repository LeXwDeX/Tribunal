---
version: 1
slug: "web-src-app-tsx"
primary_target: "web/src/app.tsx"
related_targets: ["web/src/pages"]
---

# Review Console

Mode: Operate. Implement the existing six-screen prototype and docs/design/07-web-console.md.
Audience: administrators and read-only viewers inspecting GitLab review evidence.
The API is the source of truth; fixture data belongs only in tests. No new interview
or visual choice is claimed. The brief already pins structure and visual identity.

## Direction contract

THESIS: A review is traceable from proposal through challenge to disposition. Keep
the existing working console, not a marketing dashboard or a new visual world.

OWN-WORLD: Follow the #601 "control room" direction: neutral graphite surfaces
(dark first, with a matching light theme whose sidebar is also light), one cyan
accent reserved for live status, current selection, focus and the current stage,
off-white primary actions on dark, hairline borders, Chinese sans labels, and
monospace for IDs, durations and figures. Status uses both text and color.

STORY: Find a run, inspect the execution graph, read the evidence and challenge,
then act on a failure or publication. Administrators can register projects and
maintain context; viewers cannot change state.

FIRST VIEWPORT: Desktop keeps a narrow left navigation and a wide content area.
The overview leads with current runs and required attention, followed by recent
reviews. A review detail leads with MR identity and status, a layered execution
graph, and a filterable findings table. Node selection opens its evidence drawer.
At 390px the navigation collapses; tables and the graph scroll inside their region.

FORM: Existing prototype implementation, not a concept competition. Seed not
applicable: the user-approved project specification pins the visual reference.
The signature interaction is selecting a graph node to inspect its structured
output and model-call evidence without losing the review's overall state.

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance

Unresolved: real GitLab OAuth and production data require deployment credentials.
Browser verification will use explicitly identified local fixtures, not claim a
real GitLab or LLM-provider integration. No raster assets are required.
