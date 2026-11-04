package app

import (
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestHelpReplyNamesOnlyTheReviewProfile(t *testing.T) {
	text := helpReplyText(map[string]config.ProfileConfig{
		config.ReviewProfile: {}, config.OnboardingProfile: {}, "deep": {},
	})
	for _, banned := range []string{"deep", "onboarding", "<profile>"} {
		if strings.Contains(text, banned) {
			t.Fatalf("help reply offers %q:\n%s", banned, text)
		}
	}
	if !strings.Contains(text, "/review full") || !strings.Contains(text, "`default`") {
		t.Fatalf("help reply lost the commands or the review profile:\n%s", text)
	}
}
