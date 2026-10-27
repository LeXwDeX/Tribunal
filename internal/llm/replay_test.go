package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

type replayExchange struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

type replayFixture struct {
	Exchanges []replayExchange `json:"exchanges"`
}

type replayRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type replayRecorder struct {
	t       *testing.T
	mu      sync.Mutex
	fixture replayFixture
	next    int
	seen    []replayRequest
}

func newReplayServer(t *testing.T, path string) (*httptest.Server, *replayRecorder) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var fixture replayFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	recorder := &replayRecorder{t: t, fixture: fixture}
	server := httptest.NewServer(http.HandlerFunc(recorder.ServeHTTP))
	t.Cleanup(server.Close)
	return server, recorder
}

func (r *replayRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.t.Errorf("read request body: %v", err)
	}
	r.mu.Lock()
	index := r.next
	r.next++
	r.seen = append(r.seen, replayRequest{
		Method: req.Method,
		Path:   req.URL.Path,
		Header: req.Header.Clone(),
		Body:   body,
	})
	r.mu.Unlock()
	if index >= len(r.fixture.Exchanges) {
		r.t.Errorf("unexpected request %d: %s %s", index, req.Method, req.URL.Path)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	exchange := r.fixture.Exchanges[index]
	w.Header().Set("Content-Type", "application/json")
	for name, value := range exchange.Headers {
		w.Header().Set(name, value)
	}
	w.WriteHeader(exchange.Status)
	_, _ = w.Write(exchange.Body)
}

func (r *replayRecorder) requests() []replayRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]replayRequest, len(r.seen))
	copy(out, r.seen)
	return out
}

func (r *replayRecorder) body(index int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index >= len(r.seen) {
		r.t.Fatalf("request %d missing; only %d seen", index, len(r.seen))
	}
	return string(r.seen[index].Body)
}
