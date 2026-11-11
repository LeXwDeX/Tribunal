package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A zero max_concurrency means "no local semaphore": every call must reach the
// adapter immediately (#404).
func TestRegistryZeroMaxConcurrencyHasNoSemaphore(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	cfg := chatConfig("http://127.0.0.1:1/v1", "TEST_KEY")
	cfg.MaxConcurrency = 0
	client := mustClient(t, "chat", cfg)
	provider, ok := client.(*providerClient)
	if !ok {
		t.Fatalf("client type = %T", client)
	}
	if provider.sem != nil {
		t.Fatalf("zero max_concurrency created a semaphore with capacity %d", cap(provider.sem))
	}
}

func TestRegistryZeroMaxConcurrencyRunsCallsConcurrently(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	const calls = 24
	entered := make(chan struct{}, calls)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	// Unblock every handler on failure so Close can finish; defers run last-in
	// first-out, so this must be registered after Close.
	defer releaseAll()
	cfg := chatConfig(server.URL+"/v1", "TEST_KEY")
	cfg.MaxConcurrency = 0
	client := mustClient(t, "chat", cfg)

	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.Generate(context.Background(), simpleRequest())
			errs <- err
		}()
	}
	for i := 0; i < calls; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d calls entered the adapter concurrently", i, calls)
		}
	}
	releaseAll()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
}

func TestRegistryPositiveMaxConcurrencyBlocksTheNextCall(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	entered := make(chan struct{}, 3)
	release := make(chan struct{}, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatOK))
	}))
	defer server.Close()
	// Unblock every handler on failure so Close can finish; defers run last-in
	// first-out, so this must be registered after Close.
	defer func() {
		for i := 0; i < 3; i++ {
			select {
			case release <- struct{}{}:
			default:
			}
		}
	}()
	cfg := chatConfig(server.URL+"/v1", "TEST_KEY")
	cfg.MaxConcurrency = 2
	client := mustClient(t, "chat", cfg)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.Generate(context.Background(), simpleRequest())
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the first two calls did not reach the adapter")
		}
	}
	select {
	case <-entered:
		t.Fatal("the third call ignored max_concurrency=2")
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the third call did not start after a slot was released")
	}
	release <- struct{}{}
	release <- struct{}{}
	wg.Wait()
}
