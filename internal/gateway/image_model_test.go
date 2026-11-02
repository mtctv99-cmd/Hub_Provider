package gateway

import (
	"testing"

	"sever_provider/internal/provider"
	"sever_provider/internal/scheduler"
)

// Gemini web exposes image generation as a tool, so the selected chat model does
// not gate it. An explicit image request on a chat-only Gemini model must resolve
// to the provider's image model instead of failing.
func TestImageModelForSameProvider(t *testing.T) {
	models := scheduler.NewModelRegistry()
	register := func(id, providerName string, caps ...provider.Capability) {
		t.Helper()
		if err := models.Register(scheduler.ModelSpec{ID: id, Provider: providerName, Capabilities: caps}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	register("gemini-3.8-flash", "gemini_web", provider.CapabilityChat, provider.CapabilityImage)
	register("gemini-3.1-pro", "gemini_web", provider.CapabilityChat)
	register("dall-e-3", "copilot_ws", provider.CapabilityImage)
	register("copilot-chat", "copilot_ws", provider.CapabilityChat)
	register("plain-chat", "other", provider.CapabilityChat)

	h := &Handler{Models: models}

	if got, ok := h.imageModelFor("gemini-3.1-pro"); !ok || got != "gemini-3.8-flash" {
		t.Fatalf("gemini chat model = %q,%v; want gemini-3.8-flash,true", got, ok)
	}
	// Only Gemini web mirrors the UI toggle; other providers keep their gate.
	if _, ok := h.imageModelFor("copilot-chat"); ok {
		t.Fatal("non-gemini provider must not be silently rerouted")
	}
	if _, ok := h.imageModelFor("plain-chat"); ok {
		t.Fatal("provider without an image model must not resolve")
	}
	if _, ok := h.imageModelFor("unknown-model"); ok {
		t.Fatal("unknown model must not resolve")
	}
	if h2 := (&Handler{}); func() bool { _, ok := h2.imageModelFor("gemini-3.1-pro"); return ok }() {
		t.Fatal("nil registry must not resolve")
	}
}
