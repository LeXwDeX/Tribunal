package llm

import "testing"

func TestProviderInfoModelIdentityAliasesSameEndpoint(t *testing.T) {
	first := ProviderInfo{
		Name: "alias-a", Protocol: "openai-chat", Model: "same-model",
		BaseURL: "HTTPS://Gateway.Example/v1/",
	}
	second := ProviderInfo{
		Name: "alias-b", Protocol: "openai-chat", Model: "same-model",
		BaseURL: "https://gateway.example/v1",
	}
	if first.ModelIdentity() == "" {
		t.Fatal("empty identity")
	}
	if first.ModelIdentity() != second.ModelIdentity() {
		t.Fatalf("aliases differ: %q vs %q", first.ModelIdentity(), second.ModelIdentity())
	}
	differentModel := second
	differentModel.Model = "other-model"
	if first.ModelIdentity() == differentModel.ModelIdentity() {
		t.Fatal("different model produced the same identity")
	}
	differentEndpoint := second
	differentEndpoint.BaseURL = "https://other.example/v1"
	if first.ModelIdentity() == differentEndpoint.ModelIdentity() {
		t.Fatal("different endpoint produced the same identity")
	}
	differentProtocol := second
	differentProtocol.Protocol = "anthropic"
	if first.ModelIdentity() != differentProtocol.ModelIdentity() {
		t.Fatal("identical endpoint/model aliases must share an identity regardless of protocol")
	}
	if (ProviderInfo{}).ModelIdentity() != "" {
		t.Fatal("provider without model must have no identity")
	}
}
