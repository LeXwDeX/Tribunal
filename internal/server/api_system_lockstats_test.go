package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleLockStats(t *testing.T) {
	s := &Server{lockStats: func() LockStats {
		return LockStats{
			SQLite: SQLiteLockStats{
				WaitCount:          2,
				WaitDurationMS:     15,
				MaxOpenConnections: 1,
				InUse:              0,
				Idle:               1,
			},
			Workspace: WorkspaceLockStats{
				WaitCount:      1,
				WaitDurationMS: 7,
				MaxWaitMS:      7,
			},
		}
	}}
	rec := httptest.NewRecorder()
	s.handleLockStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/lock-stats", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var got struct {
		SQLite struct {
			WaitCount          int64 `json:"wait_count"`
			WaitDurationMS     int64 `json:"wait_duration_ms"`
			MaxOpenConnections int   `json:"max_open_connections"`
			InUse              int   `json:"in_use"`
			Idle               int   `json:"idle"`
		} `json:"sqlite"`
		Workspace struct {
			WaitCount      int64 `json:"wait_count"`
			WaitDurationMS int64 `json:"wait_duration_ms"`
			MaxWaitMS      int64 `json:"max_wait_ms"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.SQLite.WaitCount != 2 || got.SQLite.WaitDurationMS != 15 ||
		got.SQLite.MaxOpenConnections != 1 || got.SQLite.InUse != 0 || got.SQLite.Idle != 1 {
		t.Fatalf("sqlite stats = %+v", got.SQLite)
	}
	if got.Workspace.WaitCount != 1 || got.Workspace.WaitDurationMS != 7 || got.Workspace.MaxWaitMS != 7 {
		t.Fatalf("workspace stats = %+v", got.Workspace)
	}
}

func TestHandleLockStatsUnavailable(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleLockStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/lock-stats", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body)
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Error.Code != "internal" {
		t.Fatalf("error code = %q, want internal", got.Error.Code)
	}
}

func TestLockStatsRouteAuthorization(t *testing.T) {
	env := newTestEnv(t)
	defer env.close()

	anonymous := env.request(http.MethodGet, "/api/v1/system/lock-stats", "", nil, nil)
	if anonymous.status != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", anonymous.status)
	}

	allowed := env.request(http.MethodGet, "/api/v1/system/lock-stats", env.adminToken, nil, nil)
	if allowed.status != http.StatusServiceUnavailable {
		t.Fatalf("admin status = %d, want 503 from the unwired provider; body=%s", allowed.status, allowed.body)
	}
}
