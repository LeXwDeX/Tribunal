package review

import (
	"strings"
	"testing"
)

// The bundled code-review skill pins its fixed point to these SHAs, so every
// review task must carry them.
func TestSummaryForPromptCarriesFixedPointSHAs(t *testing.T) {
	summary := summaryForPrompt(validDossier(t))
	for _, want := range []string{"base " + testBaseSHA, "target " + testStartSHA, "head " + testHeadSHA} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary is missing %q:\n%s", want, summary)
		}
	}
}
