package app

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/review"
)

func TestReviewProfileHashCanary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	got := f.service.reviewProfileHash(review.Profile{Name: "default", Config: f.profiles["default"]})
	const want = "a6c0782a0e5c9f77e5630fcf58d7d97cd068cd8cfe68528e379473e4e7052c6f"
	if got != want {
		t.Fatalf("profile hash = %s, want %s", got, want)
	}
}
