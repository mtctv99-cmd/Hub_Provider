package scheduler

import (
	"testing"

	"sever_provider/internal/provider"
)

func TestRegisterGeminiReasoningEffortAliases(t *testing.T) {
	models := NewModelRegistry()
	spec := ModelSpec{
		ID:           "gemini-3.8-flash",
		Provider:     "gemini_web",
		Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityReasoning},
		TimeoutSec:   90,
	}
	if err := models.RegisterWithReasoningEffortAliases(spec); err != nil {
		t.Fatal(err)
	}
	for _, effort := range []string{"low", "medium", "high"} {
		alias, ok := models.Get(spec.ID + "-" + effort)
		if !ok {
			t.Fatalf("missing %s effort alias", effort)
		}
		if alias.BaseModel != spec.ID || alias.ReasoningEffort != effort || alias.Provider != spec.Provider || alias.TimeoutSec != spec.TimeoutSec {
			t.Fatalf("unexpected %s alias: %+v", effort, alias)
		}
		gotReq := providerModelRequest(provider.ChatRequest{Model: alias.ID}, alias)
		if gotReq.Model != spec.ID || gotReq.ReasoningEffort != effort {
			t.Fatalf("alias request was not normalized: %+v", gotReq)
		}
	}
}

func TestRegisterReasoningEffortAliasesOnlyForGemini(t *testing.T) {
	models := NewModelRegistry()
	spec := ModelSpec{
		ID:           "deepseek-reasoner",
		Provider:     "deepseek_web",
		Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityReasoning},
	}
	if err := models.RegisterWithReasoningEffortAliases(spec); err != nil {
		t.Fatal(err)
	}
	if _, ok := models.Get(spec.ID + "-low"); ok {
		t.Fatal("did not expect synthetic effort levels for a provider without distinct effort support")
	}
}
