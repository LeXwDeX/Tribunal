package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/review"
)

// legacyProfileHashPayload mirrors the pre-v1.4.0 profile identity payload,
// whose user model provider configs still carried the price object.
type legacyProfileHashPayload struct {
	Profile    review.Profile
	Blocks     map[string]config.BlockBinding
	Providers  []string
	UserModels map[string]config.ProviderConfig `json:"user_models,omitempty"`
}

func legacyProfileHash(t *testing.T, s *ReviewService, profile review.Profile, modelID string, price *config.PriceConfig) (string, []byte) {
	t.Helper()
	model, ok := s.catalog.Resolve(modelID)
	if !ok {
		t.Fatalf("resolve %s", modelID)
	}
	provider := model.ProviderConfig()
	provider.Price = price
	data, err := json.Marshal(legacyProfileHashPayload{
		Profile: profile, Blocks: s.blocks, Providers: s.providerNames,
		UserModels: map[string]config.ProviderConfig{modelID: provider},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sha256Hex(string(data)), data
}

// A user model that stored a legacy price produced a different profile hash
// before v1.4.0. Removing the field is an accepted identity change: in-flight
// workflows that reference it are cancelled on recovery with a visible cause.
func TestLegacyUserModelPriceChangesProfileHash(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	attachCatalog(t, f, userModel("user:ref", "https://llm.example.test/v1", "m1", 1))
	profile := profileWithProvider("user:ref")

	current := f.service.reviewProfileHash(profile)
	if current == "" {
		t.Fatal("profile hash is empty")
	}
	priced, _ := legacyProfileHash(t, f.service, profile, "user:ref",
		&config.PriceConfig{InputPerMTok: 1.5, OutputPerMTok: 2.5})
	if priced == current {
		t.Fatal("legacy price must change the profile hash")
	}
	// A model without price keeps the byte-identical payload, so those
	// workflows are not cancelled on upgrade.
	_, unpriced := legacyProfileHash(t, f.service, profile, "user:ref", nil)
	if !bytes.Equal(unpriced, f.service.reviewProfileHashPayload(profile)) {
		t.Fatalf("unpriced model payload changed:\n got %s\nwant %s", unpriced, f.service.reviewProfileHashPayload(profile))
	}
}

func TestLegacyUserModelPriceCancelsInFlightReviewOnRecovery(t *testing.T) {
	// Run the review with the fixture's fake provider so the interruption is
	// deterministic, then simulate the upgraded service resolving a user model
	// that stored a legacy price.
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-legacy-price")
	id := interruptReview(t, f, job)

	f.profiles["default"].Propose[0].Provider = "user:ref"
	attachCatalog(t, f, userModel("user:ref", "https://llm.example.test/v1", "m1", 1))
	profile := review.Profile{Name: "default", Config: f.profiles["default"]}
	legacyHash, _ := legacyProfileHash(t, f.service, profile, "user:ref",
		&config.PriceConfig{InputPerMTok: 1.5, OutputPerMTok: 2.5})

	ctx := context.Background()
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(record.Data, &data); err != nil {
		t.Fatal(err)
	}
	metadata, ok := data["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("dossier metadata = %T", data["metadata"])
	}
	if metadata["profile_hash"] == legacyHash {
		t.Fatal("fixture already carries the legacy hash")
	}
	metadata["profile_hash"] = legacyHash
	if record.Data, err = json.Marshal(data); err != nil {
		t.Fatal(err)
	}
	record.Hash = ""
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := f.service.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	wf, err := f.db.GetWorkflow(ctx, id)
	if err != nil || wf.Status != "cancelled" {
		t.Fatalf("workflow = %+v, %v; want cancelled", wf, err)
	}
	snapshot, err := f.service.engine.Snapshot(ctx, id)
	if err != nil || snapshot.CancelReason != "profile_hash_changed" {
		t.Fatalf("cancel reason = %q, %v; want profile_hash_changed", snapshot.CancelReason, err)
	}
	if !strings.Contains(logs.String(), "cause=profile_hash_changed") {
		t.Fatalf("recovery log lacks the cause:\n%s", logs.String())
	}
}
