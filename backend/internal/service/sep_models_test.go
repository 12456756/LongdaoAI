package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type sepModelsAccounts struct {
	AccountRepository
	accounts []Account
	err      error
}

func (r *sepModelsAccounts) ListSchedulableByGroupID(context.Context, int64) ([]Account, error) {
	return r.accounts, r.err
}

type sepModelsPrices struct {
	ModelBasePriceRepository
	prices map[string]*ModelBasePrice
}

func (r *sepModelsPrices) GetBase(_ context.Context, model string) (*ModelBasePrice, error) {
	return r.prices[model], nil
}

func sepModelFixture() (*GatewayService, *APIKey, *sepModelsAccounts) {
	group := &Group{ID: 1, Platform: PlatformOpenAI, AllowImageGeneration: true}
	account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"chat-alias": "gpt-5.5", "image-alias": "gpt-image-2", "video-alias": "minimax-h3", "token-image": "gpt-image-2"}}}
	accounts := &sepModelsAccounts{accounts: []Account{account}}
	prices := &sepModelsPrices{prices: map[string]*ModelBasePrice{
		"chat-alias":  {Model: "chat-alias", Price: AccountModelPrice{Mode: BillingModeToken}},
		"image-alias": {Model: "image-alias", Price: AccountModelPrice{Mode: BillingModeImage}},
		"video-alias": {Model: "video-alias", Price: AccountModelPrice{Mode: BillingModeVideo}},
		"token-image": {Model: "token-image", Price: AccountModelPrice{Mode: BillingModeToken}},
	}}
	key := &APIKey{GroupID: &group.ID, Group: group, SEP: &SEPExecution{Kind: "chat"}}
	return &GatewayService{accountRepo: accounts, billingService: &BillingService{basePrices: prices}}, key, accounts
}

func TestSEPModelsSeparatesPurposesInMixedGroup(t *testing.T) {
	s, key, _ := sepModelFixture()
	for kind, expected := range map[string][]string{"chat": {"chat-alias"}, "image": {"image-alias", "token-image"}, "video": {"video-alias"}} {
		key.SEP.Kind = kind
		models, err := s.SEPModels(t.Context(), key)
		require.NoError(t, err)
		require.Equal(t, expected, models)
	}
}

func TestSEPModelsNeverInventsDefaultResourcesOrUsesStaleMembership(t *testing.T) {
	s, key, repo := sepModelFixture()
	models, err := s.SEPModels(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, []string{"chat-alias"}, models)
	repo.accounts = nil
	models, err = s.SEPModels(t.Context(), key)
	require.NoError(t, err)
	require.Empty(t, models)
	key.Group.ModelsListConfig = GroupModelsListConfig{Enabled: true, Models: []string{"chat-alias"}}
	models, err = s.SEPModels(t.Context(), key)
	require.NoError(t, err)
	require.Empty(t, models)
	repo.err = errors.New("database unavailable")
	_, err = s.SEPModels(t.Context(), key)
	require.ErrorContains(t, err, "database unavailable")
}

func TestSEPModelsHonorsWhitelistGroupSelectionCapabilitiesAndPrice(t *testing.T) {
	s, key, repo := sepModelFixture()
	key.Group.ModelsListConfig = GroupModelsListConfig{Enabled: true, Models: []string{"chat-alias", "forbidden"}}
	models, err := s.SEPModels(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, []string{"chat-alias"}, models)
	key.SEP.Kind = "image"
	models, err = s.SEPModels(t.Context(), key)
	require.NoError(t, err)
	require.Empty(t, models)
	key.Group.ModelsListConfig.Enabled = false
	key.SEP.Kind = "video"
	repo.accounts[0].Credentials["openai_capabilities"] = []string{"chat_completions"}
	models, err = s.SEPModels(t.Context(), key)
	require.NoError(t, err)
	require.Empty(t, models)
	key.SEP.Kind = "chat"
	delete(s.billingService.basePrices.(*sepModelsPrices).prices, "chat-alias")
	models, err = s.SEPModels(t.Context(), key)
	require.NoError(t, err)
	require.Empty(t, models)
}

func TestSEPModelsDoesNotSilentlyTruncate(t *testing.T) {
	s, key, repo := sepModelFixture()
	mapping := map[string]any{}
	prices := s.billingService.basePrices.(*sepModelsPrices).prices
	for i := range 2001 {
		model := fmt.Sprintf("chat-%04d", i)
		mapping[model] = model
		prices[model] = &ModelBasePrice{Model: model, Price: AccountModelPrice{Mode: BillingModeToken}}
	}
	repo.accounts[0].Credentials["model_mapping"] = mapping
	_, err := s.SEPModels(t.Context(), key)
	var failure *SEPError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "SEP_MODEL_DIRECTORY_TOO_LARGE", failure.Code)
}
