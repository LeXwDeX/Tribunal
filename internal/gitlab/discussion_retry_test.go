package gitlab

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestCreateDiscussionDoesNotRetryServerFailure(t *testing.T) {
	var requests atomic.Int64
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if _, err := client.CreateDiscussion(context.Background(), "token", 42, 7, NewDiscussion{Body: "body"}); StatusOf(err) != http.StatusServiceUnavailable {
		t.Fatalf("CreateDiscussion = %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("POST requests = %d", got)
	}
}
