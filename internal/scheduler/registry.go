package scheduler

import (
	"fmt"
	"sync"

	"sever_provider/internal/provider"
)

type ModelSpec struct {
	ID           string
	Provider     string
	Capabilities []provider.Capability
	FailoverTo   []string
	TimeoutSec   int
	// GuestAllowed lets a model run without any stored credential (Gemini
	// flash-lite guest mode). The adapter is called with an empty secret.
	GuestAllowed bool
}

type ModelRegistry struct {
	mu     sync.RWMutex
	models map[string]ModelSpec
}

func NewModelRegistry() *ModelRegistry { return &ModelRegistry{models: make(map[string]ModelSpec)} }
func (r *ModelRegistry) Register(spec ModelSpec) error {
	if spec.ID == "" || spec.Provider == "" {
		return fmt.Errorf("model id and provider are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[spec.ID] = spec
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
