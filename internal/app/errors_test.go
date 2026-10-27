package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestJobSettleClassNamesReasonWithoutErrorText(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"mr closed", PermanentJob(fmt.Errorf("%w: state SECRET", ErrMRClosed)), "mr_closed"},
		{"superseded", fmt.Errorf("%w: head SECRET", ErrReviewSuperseded), "review_superseded"},
		{"stale", fmt.Errorf("SECRET: %w", ErrStalePublication), "stale_publication"},
		{"coded", PermanentJob(&Error{Code: "gitlab_rejected", Message: "SECRET"}), "gitlab_rejected"},
		{"timeout", PermanentJob(fmt.Errorf("SECRET: %w", context.DeadlineExceeded)), "timeout"},
		{"permanent", PermanentJob(errors.New("build dossier: SECRET")), "permanent_job"},
		{"plain", errors.New("SECRET"), "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jobSettleClass(tc.err)
			if got != tc.want || strings.Contains(got, "SECRET") {
				t.Fatalf("jobSettleClass = %q, want %q", got, tc.want)
			}
		})
	}
}
