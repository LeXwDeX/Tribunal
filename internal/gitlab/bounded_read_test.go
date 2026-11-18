package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// countingBody streams a fixed number of bytes and records how much the
// client read, so a test can prove a bounded read stops early.
type countingBody struct {
	remaining int64
	read      int64
	closed    bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > b.remaining {
		n = b.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'a'
	}
	b.remaining -= n
	b.read += n
	return int(n), nil
}

func (b *countingBody) Close() error {
	b.closed = true
	return nil
}

type bodyTransport struct {
	status int
	body   io.ReadCloser
}

func (t *bodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: t.status,
		Header:     make(http.Header),
		Body:       t.body,
		Request:    req,
	}, nil
}

func newBodyClient(t *testing.T, status int, body io.ReadCloser) *Client {
	t.Helper()
	client, err := New(Config{BaseURL: "https://gitlab.example.com", RequestTimeout: 2 * time.Second, MaxConcurrency: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.http = &http.Client{Transport: &bodyTransport{status: status, body: body}}
	return client
}

// An oversized raw file must be rejected at the bound instead of being
// buffered whole and checked afterwards (#518).
func TestGetRawFileStopsReadingAtBound(t *testing.T) {
	body := &countingBody{remaining: 8 << 20}
	client := newBodyClient(t, http.StatusOK, body)

	_, err := client.GetRawFile(context.Background(), "token", 42, "big.txt", "main")
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("error = %v, want ErrFileTooLarge", err)
	}
	if body.read != MaxRawFileBytes+1 {
		t.Fatalf("read %d bytes, want %d (bound + 1)", body.read, MaxRawFileBytes+1)
	}
	if !body.closed {
		t.Fatal("oversized body was not closed")
	}
}

func TestGetRawFileBoundary(t *testing.T) {
	body := &countingBody{remaining: MaxRawFileBytes}
	client := newBodyClient(t, http.StatusOK, body)
	data, err := client.GetRawFile(context.Background(), "token", 42, "exact.txt", "main")
	if err != nil || len(data) != MaxRawFileBytes {
		t.Fatalf("exact-bound body = %d bytes, %v", len(data), err)
	}
	if !body.closed {
		t.Fatal("body was not closed")
	}

	body = &countingBody{remaining: MaxRawFileBytes + 1}
	client = newBodyClient(t, http.StatusOK, body)
	if _, err := client.GetRawFile(context.Background(), "token", 42, "over.txt", "main"); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("one byte over the bound = %v, want ErrFileTooLarge", err)
	}
	if body.read != MaxRawFileBytes+1 {
		t.Fatalf("read %d bytes, want %d", body.read, MaxRawFileBytes+1)
	}
}

// A failed raw file request keeps the sanitized GitLab error shape.
func TestGetRawFileMapsUpstreamStatus(t *testing.T) {
	client := newBodyClient(t, http.StatusNotFound, &countingBody{remaining: 0})
	_, err := client.GetRawFile(context.Background(), "token", 42, "missing.txt", "main")
	if !IsNotFound(err) {
		t.Fatalf("error = %v, want not found", err)
	}
	if !strings.Contains(err.Error(), "get_raw_file") {
		t.Fatalf("error = %v, want the get_raw_file operation", err)
	}
}

// A job trace larger than the cap must be truncated while streaming. The
// whole trace must never be buffered first (#528).
func TestGetJobTraceStopsReadingAtBound(t *testing.T) {
	body := &countingBody{remaining: 8 << 20}
	client := newBodyClient(t, http.StatusOK, body)

	trace, err := client.GetJobTrace(context.Background(), "token", 42, 7)
	if err != nil {
		t.Fatalf("GetJobTrace: %v", err)
	}
	if body.read != maxJobTraceBytes+1 {
		t.Fatalf("read %d bytes, want %d (bound + 1)", body.read, maxJobTraceBytes+1)
	}
	if len(trace) != maxJobTraceBytes+len(jobTraceTruncatedMarker) {
		t.Fatalf("trace = %d bytes, want %d", len(trace), maxJobTraceBytes)
	}
	if !strings.HasSuffix(trace, jobTraceTruncatedMarker) {
		t.Fatalf("trace has no truncation marker: %q", trace[len(trace)-32:])
	}
	if !body.closed {
		t.Fatal("oversized trace body was not closed")
	}
}

func TestGetJobTraceBoundary(t *testing.T) {
	body := &countingBody{remaining: maxJobTraceBytes}
	client := newBodyClient(t, http.StatusOK, body)
	trace, err := client.GetJobTrace(context.Background(), "token", 42, 7)
	if err != nil {
		t.Fatalf("GetJobTrace: %v", err)
	}
	if len(trace) != maxJobTraceBytes || strings.Contains(trace, jobTraceTruncatedMarker) {
		t.Fatalf("exact-bound trace = %d bytes, marker present: %v", len(trace), strings.Contains(trace, jobTraceTruncatedMarker))
	}
	if body.read != maxJobTraceBytes {
		t.Fatalf("read %d bytes, want %d", body.read, maxJobTraceBytes)
	}
}
