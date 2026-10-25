package store

import "time"

// Project access roles. A grant is the only way a user may read or manage one
// project's business data. The nil AccessUserID on a filter keeps the trusted
// internal, unrestricted path.
const (
	ProjectAccessRead   = "read"
	ProjectAccessManage = "manage"
)

// User is one locally known GitLab user. Disabled users cannot log in, use an
// existing session or API token, or receive new credentials.
type User struct {
	ID          int64      `json:"id"`
	Username    string     `json:"username"`
	Name        string     `json:"name"`
	Disabled    bool       `json:"disabled"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// ProjectGrant authorizes one user on one project. Username is optional and is
// filled by ListProjectGrants for display.
type ProjectGrant struct {
	ProjectID int64  `json:"project_id"`
	UserID    int64  `json:"user_id"`
	Role      string `json:"role"`
	Username  string `json:"username,omitempty"`
}
