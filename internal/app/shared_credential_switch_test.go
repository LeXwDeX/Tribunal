package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/secret"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// totalCalls counts every credential use observed by the fake. A switch must
// not call GitLab at all, so tests snapshot this before RotateToken.
func (f *sharedBotAPI) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tokenUses)
}

const storedTokenRejectedSeed = "project token was rejected by GitLab while checking the webhook; replace the project token"

// registerProjectTokenProject registers project 42 with a per-project token.
// The service is built with the shared credential configured, so the same
// instance can switch the project afterwards.
func registerProjectTokenProject(t *testing.T, ctx context.Context, st *sqlite.Store, api *sharedBotAPI, autoWebhook bool) *ProjectService {
	t.Helper()
	// The default fake identifies any unknown token as user 9, so bind that
	// user to Reporter for the per-project token's membership precheck.
	api.setAccess(projectAccessTok, botUserID, reporterAccessLevel)
	service := newSharedBotProjectService(t, st, api, sharedBotToken, nil)
	if _, err := service.Register(ctx, RegisterInput{
		Project:     "42",
		AccessToken: projectAccessTok,
		AutoWebhook: autoWebhook,
		OwnerUserID: registrantUserID,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return service
}

func TestRotateTokenEmptySwitchesHooklessProjectToSharedCredential(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := registerProjectTokenProject(t, ctx, st, api, false)

	if err := st.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		State: store.WebhookStateFailed, Error: storedTokenRejectedSeed,
	}); err != nil {
		t.Fatalf("seed failed webhook state: %v", err)
	}
	before := api.totalCalls()

	rotation, err := service.RotateToken(ctx, 42, "  \t ")
	if err != nil {
		t.Fatalf("RotateToken(empty): %v", err)
	}
	if got := api.totalCalls(); got != before {
		t.Fatalf("switch called GitLab %d times", got-before)
	}
	if rotation == nil || rotation.Project == nil {
		t.Fatalf("rotation = %+v", rotation)
	}
	if !hasWarning(rotation.Warnings, "shared_credential_unverified") {
		t.Fatalf("switch warnings = %+v, want the unverified shared credential warning", rotation.Warnings)
	}
	if hasWarning(rotation.Warnings, "shared_hook_health_unmonitored") {
		t.Fatalf("switch warned about missing provisioning although it is configured: %+v", rotation.Warnings)
	}
	if len(rotation.Project.TokenEnc) != 0 {
		t.Fatalf("rotation returned a stored token: %x", rotation.Project.TokenEnc)
	}

	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(saved.TokenEnc) != 0 {
		t.Fatalf("token sentinel = %v, want a zero-length value", saved.TokenEnc)
	}
	if saved.TokenExpiresAt != nil {
		t.Fatalf("token expiry was not cleared: %v", saved.TokenExpiresAt)
	}
	if len(saved.TokenScopes) != 0 {
		t.Fatalf("token scopes were not cleared: %v", saved.TokenScopes)
	}
	// Without a managed hook there is nothing to verify: the project must not
	// strand in "pending".
	if saved.WebhookState != store.WebhookStateDisabled || saved.WebhookError != "" {
		t.Fatalf("webhook state = %q/%q, want disabled with no error", saved.WebhookState, saved.WebhookError)
	}
	if token, err := service.tokens.Token(saved); err != nil || token != sharedBotToken {
		t.Fatalf("resolver token = %q, %v; want the shared bot", token, err)
	}
	if !service.tokens.UsesShared(saved) {
		t.Fatal("resolver does not report the project as shared")
	}

	catchUp := &CatchUpService{
		store:    st,
		webhooks: &WebhookService{store: st},
		gitlab:   &catchUpSharedAPI{sharedBotAPI: api},
		cipher:   newTestCipher(t),
		tokens:   newProjectTokenResolver(newTestCipher(t), sharedBotToken).WithProvisioning(provisioningToken),
		logger:   discardLogger(),
	}
	catchUp.ReconcileHookHealth(ctx)
	scanned, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if scanned.WebhookState != store.WebhookStateDisabled || scanned.WebhookError != "" {
		t.Fatalf("hook-less state after health scan = %q/%q, want disabled", scanned.WebhookState, scanned.WebhookError)
	}
	if api.callCount("ListProjectHooks") != 0 {
		t.Fatalf("health scan listed hooks for a hook-less project: %d", api.callCount("ListProjectHooks"))
	}

	// Switching back keeps the hook-less project disabled; the scan does not
	// invent a webhook state.
	api.setAccess("glpat-restored", botUserID, reporterAccessLevel)
	if _, err := service.RotateToken(ctx, 42, "glpat-restored"); err != nil {
		t.Fatalf("rotate back: %v", err)
	}
	restored, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(restored.TokenEnc) == 0 || restored.WebhookState != store.WebhookStateDisabled || restored.WebhookError != "" {
		t.Fatalf("project after rotate back = %+v", restored)
	}
}

func TestRotateTokenNonEmptyRestoresProjectTokenAfterSharedSwitch(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := registerProjectTokenProject(t, ctx, st, api, true)

	if _, err := service.RotateToken(ctx, 42, ""); err != nil {
		t.Fatalf("switch to shared: %v", err)
	}
	switched, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if switched.HookID == nil {
		t.Fatal("switch dropped the managed hook")
	}
	// With a managed hook the switch waits for the health scan to verify it.
	if switched.WebhookState != store.WebhookStatePending || switched.WebhookError != "" {
		t.Fatalf("shared hook state = %q/%q, want pending", switched.WebhookState, switched.WebhookError)
	}

	api.setAccess("glpat-restored", botUserID, reporterAccessLevel)
	rotation, err := service.RotateToken(ctx, 42, "glpat-restored")
	if err != nil {
		t.Fatalf("rotate back: %v", err)
	}
	if !hasWarning(rotation.Warnings, WarningWebhookRequiresMaintainer) {
		t.Fatalf("rotation warnings = %+v, want the maintainer warning", rotation.Warnings)
	}

	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	plain, err := newTestCipher(t).Open(saved.TokenEnc, secret.TokenAAD(42))
	if err != nil || string(plain) != "glpat-restored" {
		t.Fatalf("stored token = %q, %v", plain, err)
	}
	if len(saved.TokenScopes) != 2 {
		t.Fatalf("token scopes = %v, want the new token scopes", saved.TokenScopes)
	}
	if token, err := service.tokens.Token(saved); err != nil || token != "glpat-restored" {
		t.Fatalf("resolver token = %q, %v; want the project token", token, err)
	}
	if service.tokens.UsesShared(saved) {
		t.Fatal("resolver still reports the project as shared")
	}
	// Rotating back does not clear the pending mark itself; the health scan
	// re-verifies the hook with the restored project token.
	if saved.WebhookState != store.WebhookStatePending || saved.WebhookError != "" {
		t.Fatalf("hook state after rotate back = %q/%q, want pending", saved.WebhookState, saved.WebhookError)
	}
	catchUp := &CatchUpService{
		store:    st,
		webhooks: &WebhookService{store: st},
		gitlab:   &catchUpSharedAPI{sharedBotAPI: api},
		cipher:   newTestCipher(t),
		tokens:   newProjectTokenResolver(newTestCipher(t), sharedBotToken).WithProvisioning(provisioningToken),
		logger:   discardLogger(),
	}
	catchUp.ReconcileHookHealth(ctx)
	verified, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if verified.WebhookState != store.WebhookStateEnabled || verified.WebhookError != "" {
		t.Fatalf("hook state after scan = %q/%q, want enabled", verified.WebhookState, verified.WebhookError)
	}
	if tokens := api.usedTokens("ListProjectHooks"); len(tokens) == 0 || tokens[len(tokens)-1] != "glpat-restored" {
		t.Fatalf("hook list credentials = %v, want the restored project token", tokens)
	}
}

func TestRotateTokenEmptyWithoutSharedCredentialKeepsValidationError(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := newSharedBotProjectServiceWithProvisioning(t, st, api, "", "", nil)
	api.setAccess(projectAccessTok, botUserID, reporterAccessLevel)
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: projectAccessTok, OwnerUserID: registrantUserID,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := service.RotateToken(ctx, 42, "   ")
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "validation_failed" {
		t.Fatalf("error = %v, want validation_failed", err)
	}
	if appErr.Message != "access token is required" {
		t.Fatalf("message = %q", appErr.Message)
	}
	if len(appErr.Details) != 1 || appErr.Details[0].Field != "token" || appErr.Details[0].Message != "must not be empty" {
		t.Fatalf("details = %+v", appErr.Details)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	plain, err := newTestCipher(t).Open(saved.TokenEnc, secret.TokenAAD(42))
	if err != nil || string(plain) != projectAccessTok {
		t.Fatalf("token changed after a rejected switch: %q, %v", plain, err)
	}
}

func TestRotateTokenSharedSwitchIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := registerProjectTokenProject(t, ctx, st, api, true)

	for i := 0; i < 2; i++ {
		rotation, err := service.RotateToken(ctx, 42, "")
		if err != nil {
			t.Fatalf("switch %d: %v", i, err)
		}
		if rotation.Project == nil || len(rotation.Project.TokenEnc) != 0 {
			t.Fatalf("switch %d project = %+v", i, rotation.Project)
		}
		if !hasWarning(rotation.Warnings, "shared_credential_unverified") {
			t.Fatalf("switch %d warnings = %+v, want the unverified warning", i, rotation.Warnings)
		}
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(saved.TokenEnc) != 0 || saved.WebhookState != store.WebhookStatePending || saved.WebhookError != "" {
		t.Fatalf("project after repeated switch = %+v", saved)
	}
}

// A switched project is re-verified by the periodic hook health scan with the
// provisioning credential; a reachable managed hook converges pending to
// enabled and clears the previous token failure.
func TestSharedCredentialSwitchHookHealthConverges(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := registerProjectTokenProject(t, ctx, st, api, true)
	seeded, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if seeded.HookID == nil {
		t.Fatal("registration did not create the managed hook")
	}
	if err := st.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		State: store.WebhookStateFailed, Error: storedTokenRejectedSeed, Events: seeded.WebhookEvents,
	}); err != nil {
		t.Fatalf("seed failed webhook state: %v", err)
	}
	if _, err := service.RotateToken(ctx, 42, ""); err != nil {
		t.Fatalf("switch to shared: %v", err)
	}
	pending, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if pending.WebhookState != store.WebhookStatePending || pending.WebhookError != "" {
		t.Fatalf("pre-scan hook state = %q/%q, want pending", pending.WebhookState, pending.WebhookError)
	}
	api.setHooks([]gitlab.Hook{{ID: *seeded.HookID, AlertStatus: "executable"}})

	catchUp := &CatchUpService{
		store:    st,
		webhooks: &WebhookService{store: st},
		gitlab:   &catchUpSharedAPI{sharedBotAPI: api},
		cipher:   newTestCipher(t),
		tokens:   newProjectTokenResolver(newTestCipher(t), sharedBotToken).WithProvisioning(provisioningToken),
		logger:   discardLogger(),
	}
	catchUp.ReconcileHookHealth(ctx)

	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.WebhookState != store.WebhookStateEnabled || saved.WebhookError != "" {
		t.Fatalf("hook health = %q/%q, want enabled with no error", saved.WebhookState, saved.WebhookError)
	}
	tokens := api.usedTokens("ListProjectHooks")
	if len(tokens) != 1 || tokens[0] != provisioningToken {
		t.Fatalf("hook list credentials = %v, want the provisioning token", tokens)
	}
}

// Switches race with the periodic hook health scan and with review token
// reads. The row is written in one statement, so every read sees either the
// sentinel or a complete project token.
func TestConcurrentSharedSwitchHealthAndTokenReads(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := registerProjectTokenProject(t, ctx, st, api, true)
	cipher := newTestCipher(t)
	catchUp := &CatchUpService{
		store:    st,
		webhooks: &WebhookService{store: st},
		gitlab:   &catchUpSharedAPI{sharedBotAPI: api},
		cipher:   cipher,
		tokens:   newProjectTokenResolver(cipher, sharedBotToken).WithProvisioning(provisioningToken),
		logger:   discardLogger(),
	}
	review := &ReviewService{
		store:  st,
		cipher: cipher,
		tokens: newProjectTokenResolver(cipher, sharedBotToken),
		logger: discardLogger(),
	}
	api.setAccess("glpat-cycle", botUserID, reporterAccessLevel)

	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		for i := 0; i < 15; i++ {
			if _, err := service.RotateToken(ctx, 42, ""); err != nil {
				t.Errorf("switch to shared: %v", err)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 15; i++ {
			if _, err := service.RotateToken(ctx, 42, "glpat-cycle"); err != nil {
				t.Errorf("rotate back: %v", err)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 40; i++ {
			if _, token, err := review.projectToken(ctx, 42); err != nil || token == "" {
				t.Errorf("review token read = %q, %v", token, err)
				return
			}
		}
	}()

	stop := make(chan struct{})
	var scan sync.WaitGroup
	scan.Add(1)
	go func() {
		defer scan.Done()
		for {
			select {
			case <-stop:
				return
			default:
				catchUp.ReconcileHookHealth(ctx)
			}
		}
	}()
	workers.Wait()
	close(stop)
	scan.Wait()

	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(saved.TokenEnc) == 0 {
		if len(saved.TokenScopes) != 0 || saved.TokenExpiresAt != nil {
			t.Fatalf("sentinel row kept token metadata: %+v", saved)
		}
		return
	}
	plain, err := cipher.Open(saved.TokenEnc, secret.TokenAAD(42))
	if err != nil || string(plain) != "glpat-cycle" {
		t.Fatalf("stored token = %q, %v", plain, err)
	}
	if len(saved.TokenScopes) != 2 {
		t.Fatalf("project token scopes = %v", saved.TokenScopes)
	}
}

// hookListCallback runs after the fake enumerates hooks, which is after the
// scan has already read the project row.
type hookListCallback struct {
	*catchUpSharedAPI
	after func()
}

func (f *hookListCallback) ListProjectHooks(ctx context.Context, token string, projectID int64) ([]gitlab.Hook, error) {
	hooks, err := f.catchUpSharedAPI.ListProjectHooks(ctx, token, projectID)
	if f.after != nil {
		f.after()
	}
	return hooks, err
}

// A shared project with a managed hook cannot be health-checked without the
// provisioning credential. The switch says so instead of implying monitoring.
func TestRotateTokenEmptyWithoutProvisioningWarnsHookHealthUnmonitored(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(projectAccessTok, botUserID, reporterAccessLevel)
	service := newSharedBotProjectServiceWithProvisioning(t, st, api, sharedBotToken, "", nil)
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: projectAccessTok, AutoWebhook: true, OwnerUserID: registrantUserID,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	rotation, err := service.RotateToken(ctx, 42, "")
	if err != nil {
		t.Fatalf("switch to shared: %v", err)
	}
	if !hasWarning(rotation.Warnings, "shared_credential_unverified") ||
		!hasWarning(rotation.Warnings, "shared_hook_health_unmonitored") {
		t.Fatalf("switch warnings = %+v, want the unverified and unmonitored warnings", rotation.Warnings)
	}
	before := api.callCount("ListProjectHooks")

	catchUp := &CatchUpService{
		store:    st,
		webhooks: &WebhookService{store: st},
		gitlab:   &catchUpSharedAPI{sharedBotAPI: api},
		cipher:   newTestCipher(t),
		// No provisioning credential: the shared project's hook health is not
		// monitored at all.
		tokens: newProjectTokenResolver(newTestCipher(t), sharedBotToken),
		logger: discardLogger(),
	}
	catchUp.ReconcileHookHealth(ctx)
	if got := api.callCount("ListProjectHooks"); got != before {
		t.Fatalf("hook health was checked without provisioning: %d -> %d", before, got)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.WebhookState != store.WebhookStatePending || saved.WebhookError != "" {
		t.Fatalf("unmonitored hook state = %q/%q, want pending", saved.WebhookState, saved.WebhookError)
	}
}

// A hook-less project has no hook health to monitor, so switching without a
// provisioning credential must not warn about unmonitored hook health (#497).
func TestRotateTokenHooklessWithoutProvisioningSkipsUnmonitoredWarning(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	api.setAccess(projectAccessTok, botUserID, reporterAccessLevel)
	service := newSharedBotProjectServiceWithProvisioning(t, st, api, sharedBotToken, "", nil)
	if _, err := service.Register(ctx, RegisterInput{
		Project: "42", AccessToken: projectAccessTok, AutoWebhook: false, OwnerUserID: registrantUserID,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	rotation, err := service.RotateToken(ctx, 42, "")
	if err != nil {
		t.Fatalf("switch to shared: %v", err)
	}
	if !hasWarning(rotation.Warnings, "shared_credential_unverified") {
		t.Fatalf("switch warnings = %+v, want the unverified shared credential warning", rotation.Warnings)
	}
	if hasWarning(rotation.Warnings, "shared_hook_health_unmonitored") {
		t.Fatalf("hook-less switch warned about hook health: %+v", rotation.Warnings)
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.HookID != nil || saved.WebhookState != store.WebhookStateDisabled || saved.WebhookError != "" {
		t.Fatalf("hook-less switch state = %+v", saved)
	}
}

// The periodic scan reads the project before it calls GitLab. A credential
// switch that lands in between must not be overwritten by the stale
// observation, which would otherwise restore the old token failure.
func TestHookHealthScanDoesNotOverwriteConcurrentCredentialSwitch(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	api := newSharedBotAPI()
	configureSharedBot(api)
	service := registerProjectTokenProject(t, ctx, st, api, true)
	seeded, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if seeded.HookID == nil {
		t.Fatal("registration did not create the managed hook")
	}
	if err := st.UpdateProjectWebhook(ctx, 42, store.ProjectWebhookUpdate{
		State: store.WebhookStateFailed, Error: webhookTokenRejectedMessage, Events: seeded.WebhookEvents,
	}); err != nil {
		t.Fatalf("seed failed webhook state: %v", err)
	}

	switched := false
	switchingAPI := &hookListCallback{
		catchUpSharedAPI: &catchUpSharedAPI{sharedBotAPI: api},
		after: func() {
			if _, err := service.RotateToken(ctx, 42, ""); err != nil {
				t.Errorf("switch during health scan: %v", err)
				return
			}
			switched = true
		},
	}
	catchUp := &CatchUpService{
		store:    st,
		webhooks: &WebhookService{store: st},
		gitlab:   switchingAPI,
		cipher:   newTestCipher(t),
		tokens:   newProjectTokenResolver(newTestCipher(t), sharedBotToken).WithProvisioning(provisioningToken),
		logger:   discardLogger(),
	}
	catchUp.ReconcileHookHealth(ctx)

	if !switched {
		t.Fatal("the credential switch did not run during the health scan")
	}
	saved, err := st.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if saved.WebhookState != store.WebhookStatePending || saved.WebhookError != "" {
		t.Fatalf("stale health scan overwrote the switch: %q/%q", saved.WebhookState, saved.WebhookError)
	}
}
