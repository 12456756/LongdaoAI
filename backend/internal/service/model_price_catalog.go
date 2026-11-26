package service

import (
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

// Discovery uses capability lists, never a price table. Wildcards expand only
// over known capabilities; arbitrary aliases still require explicit configuration.
func PricingCatalogModels(platform string, mapping map[string]string) []string {
	known := []string{}
	switch platform {
	case PlatformOpenAI:
		for _, m := range openai.DefaultModels {
			known = append(known, m.ID)
		}
	case PlatformGemini:
		for _, m := range geminicli.DefaultModels {
			known = append(known, m.ID)
		}
	case PlatformGrok:
		for _, m := range xai.DefaultModels() {
			known = append(known, m.ID)
		}
	case PlatformAntigravity:
		for _, m := range antigravity.DefaultModels() {
			known = append(known, m.ID)
		}
	default:
		for _, m := range claude.DefaultModels {
			known = append(known, m.ID)
		}
	}
	raw := map[string]any{}
	for k, v := range mapping {
		raw[k] = v
	}
	account := &Account{Platform: platform, Credentials: map[string]any{"model_mapping": raw}}
	seen := map[string]bool{}
	for _, model := range known {
		if account.IsModelSupported(model) {
			seen[model] = true
		}
	}
	for model := range mapping {
		if !strings.ContainsAny(model, "*?") {
			seen[model] = true
		}
	}
	out := make([]string, 0, len(seen))
	for model := range seen {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}
