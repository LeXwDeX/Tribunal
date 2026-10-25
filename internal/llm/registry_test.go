package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

const chatOK = `{"id":"x","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func chatConfig(baseURL, keyEnv string) config.ProviderConfig {
	return config.ProviderConfig{
		Protocol:       "openai-chat",
		BaseURL:        baseURL,
		Model:          "test",
		APIKeyEnv:      keyEnv,
		MaxConcurrency: 1,
	}
}

func simpleRequest() *Request {
	return &Request{Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}}}
}

func TestRegistryAuthErrorIsNotRetried(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key secret-value","type":"invalid_request_error"}}`))
	}))
	defer server.Close()
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"), WithRetryBaseDelay(time.Millisecond))
	_, err := client.Generate(context.Background(), simpleRequest())
	classified := requireKind(t, err, ErrAuth)
	if classified.Status != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", classified.Status)
	}
	if calls.Load() != 1 {
		t.Fatalf("auth error must not be retried; calls=%d", calls.Load())
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error leaked credential: %v", err)
	}
}

func TestRegistryRetriesTransientAtMostFourTimes(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream unavailable: prompt contents"))
	}))
	defer server.Close()
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"), WithRetryBaseDelay(time.Millisecond))
	_, err := client.Generate(context.Background(), simpleRequest())
	requireKind(t, err, ErrTransient)
	if calls.Load() != 5 {
		t.Fatalf("expected one attempt plus four retries (SDK retry must be disabled), calls=%d", calls.Load())
	}
	if strings.Contains(err.Error(), "prompt contents") || strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("error leaked upstream body: %v", err)
	}
}

func TestRegistryRetriesRateLimited(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"), WithRetryBaseDelay(time.Millisecond))
	resp, err := client.Generate(context.Background(), simpleRequest())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
	if text := findPart(resp.Message, PartText); text == nil || text.Text != "ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestRegistryClassifiesContextTooLong(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"context_length_exceeded","type":"invalid_request_error"}}`))
	}))
	defer server.Close()
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"), WithRetryBaseDelay(time.Millisecond))
	_, err := client.Generate(context.Background(), simpleRequest())
	requireKind(t, err, ErrContextTooLong)
	if calls.Load() != 1 {
		t.Fatalf("context errors must not be retried; calls=%d", calls.Load())
	}
}

type failingTransport struct {
	calls atomic.Int32
}

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.calls.Add(1)
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

func TestRegistryRetriesTransportFailures(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	transport := &failingTransport{}
	httpClient := &http.Client{Transport: transport}
	client := mustClient(t, "chat", chatConfig("http://127.0.0.1:1/v1", "TEST_KEY"),
		WithHTTPClient(httpClient), WithRetryBaseDelay(time.Millisecond))
	_, err := client.Generate(context.Background(), simpleRequest())
	requireKind(t, err, ErrTransient)
	if transport.calls.Load() != 5 {
		t.Fatalf("expected 5 transport attempts, got %d", transport.calls.Load())
	}
}

func TestRegistryCancellationWhileWaitingForSemaphore(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	var records atomic.Int32
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"),
		WithRecorder(RecorderFunc(func(context.Context, CallRecord) { records.Add(1) })))

	firstDone := make(chan error, 1)
	go func() {
		_, err := client.Generate(context.Background(), simpleRequest())
		firstDone <- err
	}()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := client.Generate(ctx, simpleRequest())
	requireKind(t, err, ErrCanceled)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline error, got %v", err)
	}
	if records.Load() != 0 {
		t.Fatalf("canceled wait must not be recorded as a model call; records=%d", records.Load())
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first request: %v", err)
	}
}

func TestRegistryCancellationWhileWaitingForRateLimit(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	cfg := chatConfig(server.URL+"/v1", "TEST_KEY")
	cfg.RPM = 60
	client := mustClient(t, "chat", cfg)
	if _, err := client.Generate(context.Background(), simpleRequest()); err != nil {
		t.Fatalf("first request: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
		close(done)
	}()
	_, err := client.Generate(ctx, simpleRequest())
	<-done
	requireKind(t, err, ErrCanceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("rate-limited wait must not reach the provider; calls=%d", calls.Load())
	}
}

func TestRegistryEnforcesMaxConcurrency(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	cfg := chatConfig(server.URL+"/v1", "TEST_KEY")
	cfg.MaxConcurrency = 2
	client := mustClient(t, "chat", cfg)

	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.Generate(context.Background(), simpleRequest())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	if maximum.Load() > 2 {
		t.Fatalf("max concurrency exceeded: %d", maximum.Load())
	}
}

func TestRegistryRecorderMetadataAndSanitizedErrors(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key sk-secret-abc for prompt text","type":"invalid_request_error"}}`))
	}))
	defer server.Close()
	var records []CallRecord
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"),
		WithRecorder(RecorderFunc(func(_ context.Context, call CallRecord) {
			records = append(records, call)
		})))
	ctx := WithCallMeta(context.Background(), CallMeta{WorkflowID: "wf-1", NodeID: "node-1", Turn: 3})
	_, err := client.Generate(ctx, simpleRequest())
	requireKind(t, err, ErrBadRequest)
	if len(records) != 1 {
		t.Fatalf("expected one call record, got %d", len(records))
	}
	call := records[0]
	if call.Meta.WorkflowID != "wf-1" || call.Meta.NodeID != "node-1" || call.Meta.Turn != 3 {
		t.Fatalf("metadata lost: %+v", call.Meta)
	}
	if call.Provider != "chat" || call.Protocol != "openai-chat" || call.Model != "test" {
		t.Fatalf("unexpected record identity: %+v", call)
	}
	if call.Attempt != 0 || call.Err == nil {
		t.Fatalf("unexpected record: %+v", call)
	}
	for _, text := range []string{
		err.Error(),
		fmt.Sprintf("%+v", err),
		call.Err.Error(),
		fmt.Sprintf("%+v", call.Err),
	} {
		for _, secret := range []string{"sk-secret-abc", "secret-value", "prompt text", "bad key"} {
			if strings.Contains(text, secret) {
				t.Fatalf("sanitized error leaked %q: %s", secret, text)
			}
		}
		if strings.Contains(text, server.URL) {
			t.Fatalf("sanitized error leaked request URL: %s", text)
		}
	}
}

func TestGuardedHTTPClientRejectsCrossOriginRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusFound)
	}))
	defer origin.Close()
	client, err := guardedHTTPClient(origin.Client())
	if err != nil {
		t.Fatalf("guardedHTTPClient: %v", err)
	}
	_, err = client.Get(origin.URL + "/v1/chat/completions")
	if err == nil || !strings.Contains(err.Error(), "another origin") {
		t.Fatalf("expected cross-origin redirect rejection, got %v", err)
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("target received %d calls", targetCalls.Load())
	}
}

func TestRegistryRejectsCrossOriginRedirectFromAdapter(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusFound)
	}))
	defer origin.Close()
	client := mustClient(t, "chat", chatConfig(origin.URL+"/v1", "TEST_KEY"))
	_, err := client.Generate(context.Background(), simpleRequest())
	if err == nil {
		t.Fatal("expected redirect error")
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("target received %d calls", targetCalls.Load())
	}
}

func TestRegistryRequestOverridesProviderDefaults(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	bodies := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies <- string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	cfg := chatConfig(server.URL+"/v1", "TEST_KEY")
	cfg.MaxTokens = 100
	cfg.Temperature = floatPtr(0.4)
	client := mustClient(t, "chat", cfg)
	request := simpleRequest()
	request.MaxTokens = 200
	if _, err := client.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body := <-bodies
	assertContains(t, body, `"max_tokens":200`)
	assertContains(t, body, `"temperature":0.4`)

	request = simpleRequest()
	request.Temperature = floatPtr(0.9)
	if _, err := client.Generate(context.Background(), request); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	body = <-bodies
	assertContains(t, body, `"max_tokens":100`)
	assertContains(t, body, `"temperature":0.9`)
}

func TestNewRegistryValidation(t *testing.T) {
	if _, err := NewRegistry(map[string]config.ProviderConfig{
		"bad": {Protocol: "openai-chat", Model: "m"},
	}); err == nil || !strings.Contains(err.Error(), "api_key_env") {
		t.Fatalf("expected api_key_env error, got %v", err)
	}
	if _, err := NewRegistry(map[string]config.ProviderConfig{
		"bad": {Protocol: "openai-chat", Model: "m", APIKeyEnv: "MISSING_KEY_FOR_TEST"},
	}); err == nil || !strings.Contains(err.Error(), "MISSING_KEY_FOR_TEST") {
		t.Fatalf("expected missing env error, got %v", err)
	}
	t.Setenv("PRESENT_KEY", "value")
	if _, err := NewRegistry(map[string]config.ProviderConfig{
		"bad": {Protocol: "nope", Model: "m", APIKeyEnv: "PRESENT_KEY"},
	}); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("expected protocol error, got %v", err)
	}
	if _, err := NewRegistry(map[string]config.ProviderConfig{
		"bad": {Protocol: "openai-chat", Model: "", APIKeyEnv: "PRESENT_KEY"},
	}); err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected model error, got %v", err)
	}
	if _, err := NewRegistry(map[string]config.ProviderConfig{
		"bad": {Protocol: "openai-chat", Model: "m", APIKeyEnv: "PRESENT_KEY", BaseURL: "ftp://example.com"},
	}); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("expected base_url error, got %v", err)
	}
	if _, err := NewRegistry(nil); err != nil {
		t.Fatalf("empty registry: %v", err)
	}
	registry, err := NewRegistry(map[string]config.ProviderConfig{
		"b": {Protocol: "openai-chat", Model: "m", APIKeyEnv: "PRESENT_KEY"},
		"a": {Protocol: "anthropic", Model: "m", APIKeyEnv: "PRESENT_KEY", MaxTokens: 1},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	infos := registry.Infos()
	if len(infos) != 2 || infos[0].Name != "a" || infos[1].Name != "b" {
		t.Fatalf("Infos must be sorted: %+v", infos)
	}
	if _, err := registry.Client("missing"); err == nil {
		t.Fatal("expected unknown provider error")
	}
}

func TestParseRetryAfter(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "2")
	if got := parseRetryAfter(header); got != 2*time.Second {
		t.Fatalf("expected 2s, got %s", got)
	}
	header.Set("Retry-After", "invalid")
	if got := parseRetryAfter(header); got != 0 {
		t.Fatalf("expected 0, got %s", got)
	}
	classified := apiError(http.StatusTooManyRequests, "", "rate_limit_error", "", header, nil)
	if classified.RetryAfter != 0 {
		t.Fatalf("expected no retry-after for invalid header, got %s", classified.RetryAfter)
	}
}

func TestClassificationDoesNotRequireBody(t *testing.T) {
	classified := apiError(http.StatusBadGateway, "", "server_error", "", nil, nil)
	if classified.Kind != ErrTransient {
		t.Fatalf("expected transient, got %s", classified.Kind)
	}
	overloaded := apiError(http.StatusTooManyRequests, "", "overloaded_error", "", nil, nil)
	if overloaded.Kind != ErrOverloaded {
		t.Fatalf("expected overloaded, got %s", overloaded.Kind)
	}
}

func TestRegistryCanceledBeforeCall(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Generate(ctx, simpleRequest())
	requireKind(t, err, ErrCanceled)
	if calls.Load() != 0 {
		t.Fatalf("provider should not be called; calls=%d", calls.Load())
	}
}

func TestBackoffHonorsRetryAfterAndCap(t *testing.T) {
	client := &providerClient{baseDelay: time.Millisecond}
	if got := client.backoff(0, &Error{RetryAfter: 2 * time.Second}); got != 2*time.Second {
		t.Fatalf("expected RetryAfter 2s, got %s", got)
	}
	if got := client.backoff(20, nil); got != 4*time.Second {
		t.Fatalf("expected exponential cap 4s, got %s", got)
	}
	if got := client.backoff(0, &Error{RetryAfter: 2 * time.Minute}); got != maxRetryDelay {
		t.Fatalf("expected RetryAfter cap %s, got %s", maxRetryDelay, got)
	}
}

func TestRegistryRejectsInvalidRequest(t *testing.T) {
	t.Setenv("TEST_KEY", "secret-value")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	client := mustClient(t, "chat", chatConfig(server.URL+"/v1", "TEST_KEY"))
	_, err := client.Generate(context.Background(), nil)
	requireKind(t, err, ErrBadRequest)
	_, err = client.Generate(context.Background(), &Request{})
	requireKind(t, err, ErrBadRequest)
	_, err = client.Generate(context.Background(), &Request{Messages: []Message{{
		Role:  RoleUser,
		Parts: []Part{{Type: PartToolCall, ToolCall: &ToolCall{ID: "x", Name: "y"}}},
	}}})
	requireKind(t, err, ErrBadRequest)
}

func TestRequestJSONTags(t *testing.T) {
	raw, err := json.Marshal(Request{
		Messages: []Message{{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"messages"`) || !strings.Contains(string(raw), `"parts"`) {
		t.Fatalf("unexpected JSON: %s", raw)
	}
}
