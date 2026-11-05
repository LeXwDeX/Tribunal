package app

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/review"
)

func TestReviewProfileHashCanary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	got := f.service.reviewProfileHash(review.Profile{Name: "default", Config: f.profiles["default"]})
	const want = "2ed94c4789723b975d47ee19c86e2a81b16f0adac01fe8a1d410eca655eac08a"
	if got != want {
		t.Fatalf("profile hash = %s, want %s", got, want)
	}
}
