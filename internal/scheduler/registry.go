package scheduler

import (
	"fmt"
	"sync"

	"sever_provider/internal/provider"
)

type ModelSpec struct {
	ID              string
	Provider        string
	Capabilities    []provider.Capability
	FailoverTo      []string
	TimeoutSec      int
	BaseModel       string
	ReasoningEffort string
	// GuestAllowed lets a model run without any stored credential (Gemini
	// flash-lite guest mode). The adapter is called with an empty secret.
	GuestAllowed bool
}

type ModelRegistry struct {
	mu     sync.RWMutex
	models map[string]ModelSpec
}

func DefaultDeepSeekChatModelSpec() ModelSpec {
	return ModelSpec{
		ID:           "deepseek-chat",
		Provider:     "deepseek_web",
		Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming},
		TimeoutSec:   120,
	}
}

func DefaultDeepSeekReasonerModelSpec() ModelSpec {
	return ModelSpec{
		ID:           "deepseek-reasoner",
		Provider:     "deepseek_web",
		Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityReasoning},
		TimeoutSec:   180,
	}
}

func NewModelRegistry() *ModelRegistry {
	r := &ModelRegistry{models: make(map[string]ModelSpec)}
	_ = r.Register(DefaultDeepSeekChatModelSpec())
	_ = r.Register(DefaultDeepSeekReasonerModelSpec())
	_ = r.Register(ModelSpec{ID: "deepseek-flash", Provider: "deepseek_web", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 120})
	_ = r.Register(ModelSpec{ID: "deepseek-v4.1-flash", Provider: "deepseek_web", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 120})
	_ = r.Register(ModelSpec{ID: "deepseek-v4-flash", Provider: "deepseek_web", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming}, TimeoutSec: 120})
	_ = r.Register(ModelSpec{ID: "deepseek-v4-pro", Provider: "deepseek_web", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityReasoning}, TimeoutSec: 180})
	_ = r.Register(ModelSpec{ID: "gpt-5.6", Provider: "copilot_ws", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityVision}, TimeoutSec: 90})
	_ = r.Register(ModelSpec{ID: "gpt-5.6-reasoner", Provider: "copilot_ws", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityReasoning, provider.CapabilityVision}, TimeoutSec: 180})
	_ = r.Register(ModelSpec{ID: "copilot-chat", Provider: "copilot_ws", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityVision}, TimeoutSec: 90})
	_ = r.Register(ModelSpec{ID: "copilot-creative", Provider: "copilot_ws", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityVision}, TimeoutSec: 90})
	_ = r.Register(ModelSpec{ID: "copilot-precise", Provider: "copilot_ws", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityVision}, TimeoutSec: 90})
	_ = r.Register(ModelSpec{ID: "copilot-balanced", Provider: "copilot_ws", Capabilities: []provider.Capability{provider.CapabilityChat, provider.CapabilityStreaming, provider.CapabilityVision}, TimeoutSec: 90})
	_ = r.Register(ModelSpec{ID: "dall-e-3", Provider: "copilot_ws", Capabilities: []provider.Capability{provider.CapabilityImage}, TimeoutSec: 120})
	return r
}
func (r *ModelRegistry) Register(spec ModelSpec) error {
	if spec.ID == "" || spec.Provider == "" {
		return fmt.Errorf("model id and provider are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[spec.ID] = spec
	return nil
}

// RegisterWithReasoningEffortAliases adds low, medium, and high aliases for
// Gemini Web reasoning models while keeping other providers' model IDs intact.
func (r *ModelRegistry) RegisterWithReasoningEffortAliases(spec ModelSpec) error {
	if err := r.Register(spec); err != nil {
		return err
	}
	if spec.Provider != "gemini_web" || !contains(spec.Capabilities, provider.CapabilityReasoning) {
		return nil
	}
	for _, effort := range []string{"low", "medium", "high"} {
		alias := spec
		alias.ID = spec.ID + "-" + effort
		alias.BaseModel = spec.ID
		alias.ReasoningEffort = effort
		alias.Capabilities = make([]provider.Capability, 0, len(spec.Capabilities))
		for _, capability := range spec.Capabilities {
			if capability != provider.CapabilityImage {
				alias.Capabilities = append(alias.Capabilities, capability)
			}
		}
		if err := r.Register(alias); err != nil {
			return err
		}
	}
	return nil
}

func (r *ModelRegistry) Get(id string) (ModelSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	spec, ok := r.models[id]
	return spec, ok
}
func (r *ModelRegistry) List() []ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelSpec, 0, len(r.models))
	for _, spec := range r.models {
		out = append(out, spec)
	}
	return out
}
func (r *ModelRegistry) HasCapability(id string, cap provider.Capability) bool {
	spec, ok := r.Get(id)
	if !ok {
		return false
	}
	for _, item := range spec.Capabilities {
		if item == cap {
			return true
		}
	}
	return false
}
