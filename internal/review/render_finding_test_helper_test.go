package review

import (
	"fmt"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

// renderFindingForTest reproduces the single-finding renderer used before
// inline discussions converged on diff positions (#487). It has no production
// caller: RenderPositionThread owns publication now. The helper stays in the
// test package so the existing renderer cases keep covering writeFindingSection
// without depending on a position key or a generation marker.
func renderFindingForTest(group Group, opts FindingRenderOptions) (string, error) {
	if !group.Published() {
		return "", invalid("decision", "only publish or mark groups can be rendered inline")
	}
	if !group.PositionOK {
		return "", invalid("position_ok", "finding has no diff hunk and must be published in the summary only")
	}
	key := group.PublishKey
	if key == "" {
		key = PublishKeyFor(group.Path, group.Title, group.Evidence)
	}
	if !publishKeyRE.MatchString(key) {
		return "", invalid("publish_key", "must be a stable pk_ key")
	}

	var head strings.Builder
	fmt.Fprintf(&head, "%s key=%s -->\n", FindingMarkerPrefix, key)

	budget := newNoteBudget(0)
	budget.write(head.String())
	writeFindingSection(budget, group, opts, true)
	return limitPublishedNote(gitlab.NeutralizeQuickActions(budget.finish(""))), nil
}
