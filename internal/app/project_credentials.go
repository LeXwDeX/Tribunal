package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// Credential resolution failures stay distinct so operators can tell one
// broken project token from every shared-bot project at once. Neither message
// contains a token value.
var (
	// ErrProjectTokenUnavailable marks a project whose stored token cannot be
	// opened. Only that project is affected.
	ErrProjectTokenUnavailable = errors.New("app: project access token is unavailable")
	// ErrSharedBotCredentialUnavailable marks a project with no project token
	// while shared-credential mode is not configured. Every such project is
	// affected by the same configuration gap.
	ErrSharedBotCredentialUnavailable = errors.New("app: shared bot credential is unavailable or not configured; the project has no access token")
)

// projectTokenResolver picks the GitLab credential for a project. A project
// token always wins; an empty token sentinel falls back to the instance-wide
// shared bot. A nil resolver behaves like a resolver without a shared token.
type projectTokenResolver struct {
	cipher *secret.Cipher
	shared string
}

func newProjectTokenResolver(cipher *secret.Cipher, shared string) *projectTokenResolver {
	return &projectTokenResolver{cipher: cipher, shared: shared}
}

// HasShared reports whether shared-credential mode is enabled.
func (r *projectTokenResolver) HasShared() bool {
	return r != nil && r.shared != ""
}

// Shared returns the shared bot token, or an empty string when unconfigured.
func (r *projectTokenResolver) Shared() string {
	if r == nil {
		return ""
	}
	return r.shared
}

// UsesShared reports whether a project has no project-level token and would
// therefore call GitLab as the shared bot.
func (r *projectTokenResolver) UsesShared(project *store.Project) bool {
	return project != nil && len(project.TokenEnc) == 0 && r.HasShared()
}

// Token returns the credential to use for the project.
func (r *projectTokenResolver) Token(project *store.Project) (string, error) {
	if project == nil {
		return "", ErrProjectTokenUnavailable
	}
	if len(project.TokenEnc) > 0 {
		if r == nil || r.cipher == nil {
			return "", ErrProjectTokenUnavailable
		}
		plaintext, err := r.cipher.Open(project.TokenEnc, secret.TokenAAD(project.ID))
		if err != nil || len(plaintext) == 0 {
			return "", ErrProjectTokenUnavailable
		}
		return string(plaintext), nil
	}
	if r.HasShared() {
		return r.shared, nil
	}
	return "", ErrSharedBotCredentialUnavailable
}

// resolveProjectToken uses the service's resolver. Tests and a few legacy
// call sites build services as struct literals without a resolver; those keep
// the per-project-only behavior from their cipher.
func resolveProjectToken(tokens *projectTokenResolver, cipher *secret.Cipher, project *store.Project) (string, error) {
	if tokens != nil {
		return tokens.Token(project)
	}
	return newProjectTokenResolver(cipher, "").Token(project)
}

// Shared bot credential health states. They are reported by the lazy health
// probe; the token value itself is never part of the result.
const (
	SharedBotStateNotConfigured   = "not_configured"
	SharedBotStateOK              = "ok"
	SharedBotStateExpired         = "expired"
	SharedBotStateInsufficient    = "insufficient_scope"
	SharedBotStateUnreachable     = "unreachable"
	sharedBotHealthCacheTTL       = 5 * time.Minute
	sharedBotHealthRequestTimeout = 10 * time.Second
)

// SharedBotCredentialHealth is a cached, sanitized view of the shared bot
// credential. ExpiresAt is the credential's GitLab expiry, not the cache time.
type SharedBotCredentialHealth struct {
	Configured bool       `json:"configured"`
	State      string     `json:"state"`
	Username   string     `json:"username,omitempty"`
	ExpiresAt  *time.Time `json:"token_expires_at,omitempty"`
	CheckedAt  time.Time  `json:"checked_at"`
}

// SharedBotCredentialConfigured reports whether shared-credential mode is
// enabled without contacting GitLab.
func (s *ProjectService) SharedBotCredentialConfigured() bool {
	return s != nil && s.tokens.HasShared()
}

// SharedBotCredentialHealth returns the cached shared-credential state. The
// first call after startup or after the five-minute TTL probes GitLab with
// GetTokenInfo and CurrentUser. Concurrent cache misses share one in-flight
// probe; watchers wait on a channel rather than starting a resident goroutine.
// Any caller cancellation propagates as unreachable and is not cached.
func (s *ProjectService) SharedBotCredentialHealth(ctx context.Context) SharedBotCredentialHealth {
	if !s.SharedBotCredentialConfigured() {
		return SharedBotCredentialHealth{State: SharedBotStateNotConfigured}
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	current := now()
	s.sharedHealthMu.Lock()
	if s.sharedHealth != nil && current.Sub(s.sharedHealth.CheckedAt) < sharedBotHealthCacheTTL {
		cached := *s.sharedHealth
		s.sharedHealthMu.Unlock()
		return cached
	}
	if flight := s.sharedHealthFlight; flight != nil {
		s.sharedHealthMu.Unlock()
		select {
		case <-flight.done:
			return flight.health
		case <-ctx.Done():
			// The caller gave up; the shared probe result is not this caller's
			// to cache, and the owner still reports the real outcome.
			return SharedBotCredentialHealth{Configured: true, State: SharedBotStateUnreachable, CheckedAt: current}
		}
	}
	flight := &sharedBotHealthFlight{done: make(chan struct{})}
	s.sharedHealthFlight = flight
	s.sharedHealthMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, sharedBotHealthRequestTimeout)
	health := s.probeSharedBotCredential(probeCtx, current)
	cancel()
	if ctx.Err() != nil {
		// A caller cancellation says nothing about the credential.
		health = SharedBotCredentialHealth{Configured: true, State: SharedBotStateUnreachable, CheckedAt: current}
	} else {
		s.sharedHealthMu.Lock()
		s.sharedHealth = &health
		s.sharedHealthMu.Unlock()
	}
	flight.health = health
	s.sharedHealthMu.Lock()
	s.sharedHealthFlight = nil
	s.sharedHealthMu.Unlock()
	close(flight.done)
	return health
}

// sharedBotHealthFlight is one in-flight shared-credential probe. health is
// written once before done is closed; waiters read it only after the close.
type sharedBotHealthFlight struct {
	done   chan struct{}
	health SharedBotCredentialHealth
}

func (s *ProjectService) probeSharedBotCredential(ctx context.Context, now time.Time) SharedBotCredentialHealth {
	health := SharedBotCredentialHealth{Configured: true, State: SharedBotStateUnreachable, CheckedAt: now}
	if s.gitlab == nil {
		return health
	}
	token := s.tokens.Shared()
	info, err := s.gitlab.GetTokenInfo(ctx, token)
	if err != nil {
		switch gitlab.StatusOf(err) {
		case http.StatusUnauthorized:
			health.State = SharedBotStateExpired
		case http.StatusForbidden:
			health.State = SharedBotStateInsufficient
		}
		return health
	}
	if info == nil {
		return health
	}
	health.ExpiresAt = info.ExpiresAt
	if !info.Active || info.Revoked || tokenInfoExpired(info, now) {
		health.State = SharedBotStateExpired
		return health
	}
	if missingTokenScope(info) != "" {
		health.State = SharedBotStateInsufficient
		return health
	}
	user, err := s.gitlab.CurrentUser(ctx, token)
	if err != nil || user == nil || user.ID <= 0 || user.Username == "" {
		return health
	}
	health.Username = user.Username
	health.State = SharedBotStateOK
	return health
}

// tokenInfoExpired applies the same whole-UTC-day rule as validateTokenInfo.
func tokenInfoExpired(info *gitlab.TokenInfo, now time.Time) bool {
	return info.ExpiresAt != nil &&
		now.UTC().Format("2006-01-02") > info.ExpiresAt.UTC().Format("2006-01-02")
}

// missingTokenScope returns the first required scope the shared token lacks.
func missingTokenScope(info *gitlab.TokenInfo) string {
	scopes := make(map[string]bool, len(info.Scopes))
	for _, scope := range info.Scopes {
		scopes[scope] = true
	}
	if !scopes["api"] {
		return "api"
	}
	if !scopes["read_repository"] {
		return "read_repository"
	}
	return ""
}
