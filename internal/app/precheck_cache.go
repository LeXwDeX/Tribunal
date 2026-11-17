package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

// ttlCache is a small in-process cache for shared precheck results. Entries
// expire after a fixed TTL; a nil or zero-TTL cache is a no-op so services
// built as struct literals in older tests keep probing GitLab.
type ttlCache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	value     V
	expiresAt time.Time
}

func newTTLCache[V any](ttl time.Duration, now func() time.Time) *ttlCache[V] {
	if ttl <= 0 || now == nil {
		return nil
	}
	return &ttlCache[V]{ttl: ttl, now: now, entries: map[string]ttlEntry[V]{}}
}

func (c *ttlCache[V]) get(key string) (V, bool) {
	var zero V
	if c == nil {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return zero, false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return zero, false
	}
	return entry.value, true
}

func (c *ttlCache[V]) put(key string, value V) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.entries[key] = ttlEntry[V]{value: value, expiresAt: now.Add(c.ttl)}
	if len(c.entries) > 1024 {
		for entryKey, entry := range c.entries {
			if !now.Before(entry.expiresAt) {
				delete(c.entries, entryKey)
			}
		}
	}
}

func (c *ttlCache[V]) deletePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if strings.HasPrefix(key, prefix) {
			delete(c.entries, key)
		}
	}
}

func (c *ttlCache[V]) deleteWhere(match func(V) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if match(entry.value) {
			delete(c.entries, key)
		}
	}
}

// sharedMembershipResult caches the shared bot identity and its access level
// on one project, matching the pair Validate returns.
type sharedMembershipResult struct {
	user   *gitlab.User
	access int
}

// cachedValidation carries the project id alongside the result so a project
// write can invalidate entries whose original input form is unknown.
type cachedValidation struct {
	projectID int64
	result    ProjectValidation
}

// copyValidationResult detaches the slices a caller could otherwise share
// with the cache.
func copyValidationResult(result ProjectValidation) ProjectValidation {
	result.TokenScopes = append([]string(nil), result.TokenScopes...)
	result.Warnings = append([]ProjectWarning(nil), result.Warnings...)
	return result
}

// credentialFingerprint keys cached results without holding or logging the
// credential itself.
func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:16])
}

func projectCachePrefix(projectID int64) string {
	return "p" + strconv.FormatInt(projectID, 10) + "|"
}

func sharedMembershipCacheKey(projectID int64, sharedToken string) string {
	return projectCachePrefix(projectID) + "shared_bot_membership|" + credentialFingerprint(sharedToken)
}

func registrantMembershipCacheKey(projectID, userID int64, sharedToken string) string {
	return projectCachePrefix(projectID) + "registrant_membership|" +
		strconv.FormatInt(userID, 10) + "|" + credentialFingerprint(sharedToken)
}

func provisioningCredentialCacheKey(token string) string {
	return "global|provisioning_credential|" + credentialFingerprint(token)
}

func sharedTokenInfoCacheKey(token string) string {
	return "global|shared_token_info|" + credentialFingerprint(token)
}

func validationCacheKey(project string, ownerUserID int64, trusted bool, token string) string {
	trustedFlag := "0"
	if trusted {
		trustedFlag = "1"
	}
	return "validate|" + project + "|" + strconv.FormatInt(ownerUserID, 10) + "|" +
		trustedFlag + "|" + credentialFingerprint(token)
}

// InvalidatePrecheckCache drops cached shared precheck results for one
// project. Registration and credential or enabled-state changes call it so a
// write never leaves a stale membership or validation result behind.
func (s *ProjectService) InvalidatePrecheckCache(projectID int64) {
	if s == nil || projectID <= 0 {
		return
	}
	prefix := projectCachePrefix(projectID)
	s.sharedMembershipCache.deletePrefix(prefix)
	s.registrantCache.deletePrefix(prefix)
	s.validationCache.deleteWhere(func(value cachedValidation) bool {
		return value.projectID == projectID
	})
}

// sharedTokenInfo probes the shared bot token once per TTL. Registration and
// validation both use it; callers keep their own error handling.
func (s *ProjectService) sharedTokenInfo(ctx context.Context) (*gitlab.TokenInfo, error) {
	token := s.tokens.Shared()
	key := sharedTokenInfoCacheKey(token)
	if cached, ok := s.sharedTokenInfoCache.get(key); ok {
		return copyTokenInfo(&cached), nil
	}
	info, err := s.gitlab.GetTokenInfo(ctx, token)
	if err != nil || info == nil {
		return info, err
	}
	s.sharedTokenInfoCache.put(key, *copyTokenInfo(info))
	return info, nil
}

func copyTokenInfo(info *gitlab.TokenInfo) *gitlab.TokenInfo {
	if info == nil {
		return nil
	}
	copied := *info
	copied.Scopes = append([]string(nil), info.Scopes...)
	return &copied
}
