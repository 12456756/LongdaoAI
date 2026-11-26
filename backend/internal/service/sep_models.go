package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SEP discovery is an authorization set, so it never falls back to a static
// catalog or cached group membership after a resource has been withdrawn.
func (s *GatewayService) SEPModels(ctx context.Context, key *APIKey) ([]string, error) {
	if key == nil || key.SEP == nil || key.Group == nil || key.GroupID == nil || *key.GroupID != key.Group.ID {
		return nil, SEPFailure(403, "SEP_GROUP_REQUIRED", "企业 Key 分组不可用", "models")
	}
	group := key.Group
	accounts, err := s.accountRepo.ListSchedulableByGroupID(ctx, group.ID)
	if err != nil {
		return nil, fmt.Errorf("read enterprise model resources: %w", err)
	}
	if s.billingService == nil || !s.billingService.UsesManagedBasePrices() {
		return nil, SEPFailure(503, "SEP_PRICING_UNAVAILABLE", "SEP 模型基础价格服务不可用", "models")
	}
	candidates := map[string]bool{}
	for i := range accounts {
		a := &accounts[i]
		if a.Platform != group.Platform {
			continue
		}
		mapping := a.GetModelMapping()
		// Only expand a configured whitelist. An empty mapping is permissive
		// routing, not evidence that every static default is actually offered.
		if len(mapping) > 0 {
			for _, model := range PricingCatalogModels(a.Platform, mapping) {
				candidates[model] = true
			}
		}
	}
	if group.CustomModelsListEnabled() {
		for _, model := range group.ModelsListConfig.Models {
			if !strings.ContainsAny(model, "*?") {
				candidates[model] = true
			}
		}
	}
	models := make([]string, 0)
	for model := range candidates {
		if group.CustomModelsListEnabled() && !customModelsListAllowsModel(group.ModelsListConfig.Models, model) {
			continue
		}
		if s.channelService != nil {
			restricted, err := s.channelService.SEPModelRestricted(ctx, group.ID, model)
			if err != nil {
				return nil, err
			}
			if restricted {
				continue
			}
		}
		price, err := s.billingService.basePrices.GetBase(ctx, model)
		if err != nil {
			return nil, fmt.Errorf("read enterprise model base price: %w", err)
		}
		if price == nil {
			continue
		}
		for i := range accounts {
			a := &accounts[i]
			if a.Platform == group.Platform && a.IsModelSupported(model) && sepModelPurpose(a, model, price) == key.SEP.Kind && sepAccountSupportsPurpose(a, group, key.SEP.Kind) {
				models = append(models, model)
				break
			}
		}
	}
	sort.Strings(models)
	if len(models) > 2000 {
		return nil, SEPFailure(409, "SEP_MODEL_DIRECTORY_TOO_LARGE", "企业可用模型超过 2000 个，请缩小分组模型范围后重试", "models")
	}
	return models, nil
}

func (s *ChannelService) SEPModelRestricted(ctx context.Context, groupID int64, model string) (bool, error) {
	lk, err := s.lookupGroupChannel(ctx, groupID)
	if err != nil {
		return false, fmt.Errorf("read enterprise channel permissions: %w", err)
	}
	if lk == nil {
		return false, nil
	}
	return checkRestricted(lk, groupID, model), nil
}

func sepModelPurpose(account *Account, model string, price *ModelBasePrice) string {
	if price.Price.Mode == BillingModeImage {
		return "image"
	}
	if price.Price.Mode == BillingModeVideo {
		return "video"
	}
	for _, name := range []string{model, account.GetMappedModel(model)} {
		name = strings.ToLower(name)
		if isOpenAIImageModel(name) || isRawChatImageModel(name) || strings.HasPrefix(name, "dall-e-") {
			return "image"
		}
		if isGrokVideoBillingModel(name) || strings.HasPrefix(name, "sora") || strings.HasPrefix(name, "minimax-hailuo") || strings.HasPrefix(name, "minimax-h3") || strings.HasPrefix(name, "veo-") {
			return "video"
		}
	}
	return "chat"
}

func sepAccountSupportsPurpose(account *Account, group *Group, kind string) bool {
	switch kind {
	case "image":
		return GroupAllowsImageGeneration(group) && account.SupportsOpenAIImageCapability(OpenAIImagesCapabilityBasic)
	case "video":
		return GroupAllowsImageGeneration(group) && account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityVideos)
	case "chat":
		return !account.IsOpenAICompatible() || account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions)
	default:
		return false
	}
}
