package server

import "net/http"

// LockStats is the runtime lock-wait snapshot served to platform operators.
// Durations are milliseconds because the endpoint is consumed by scripts.
type LockStats struct {
	SQLite    SQLiteLockStats    `json:"sqlite"`
	Workspace WorkspaceLockStats `json:"workspace"`
}

// SQLiteLockStats reports database/sql connection-pool waits. The store opens
// one connection, so a wait is time queued behind it for any SQL statement,
// not a driver busy-handler retry.
type SQLiteLockStats struct {
	WaitCount          int64 `json:"wait_count"`
	WaitDurationMS     int64 `json:"wait_duration_ms"`
	MaxOpenConnections int   `json:"max_open_connections"`
	InUse              int   `json:"in_use"`
	Idle               int   `json:"idle"`
}

// WorkspaceLockStats reports waits on the per-project content, lifecycle and
// mirror locks.
type WorkspaceLockStats struct {
	WaitCount      int64 `json:"wait_count"`
	WaitDurationMS int64 `json:"wait_duration_ms"`
	MaxWaitMS      int64 `json:"max_wait_ms"`
}

func (s *Server) handleLockStats(w http.ResponseWriter, r *http.Request) {
	if s.lockStats == nil {
		writeError(w, r, http.StatusServiceUnavailable, "internal", "lock statistics are not available", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.lockStats())
}
