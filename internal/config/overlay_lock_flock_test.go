//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package config

import (
	"bytes"
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestOverlayWriterLockTimeout(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	lockPath := harness.overlay + ".lock"
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)

	before := harness.read()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = harness.writer.Apply(ctx, harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
	))
	requireOverlayError(t, err, OverlayErrorBusy)
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("overlay changed after a lock timeout")
	}
}

func TestOverlayWriterInProcessLockTimeout(t *testing.T) {
	harness := newOverlayHarness(t, overlayTestBase, overlayTestDocument)
	harness.writer.sem <- struct{}{}
	defer func() { <-harness.writer.sem }()
	before := harness.read()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := harness.writer.Apply(ctx, harness.request(
		OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 1},
	))
	requireOverlayError(t, err, OverlayErrorBusy)
	if !bytes.Equal(before, harness.read()) {
		t.Fatal("overlay changed after an in-process lock timeout")
	}
}
