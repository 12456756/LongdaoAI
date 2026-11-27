//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type sepReceiptSettings struct{ service.SettingRepository }

func (sepReceiptSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{service.SettingKeyPlatformUSDToCNYRate: "7.15"}, nil
}

func TestSEPPricingKeepsCompleteMediaReceipt(t *testing.T) {
	for _, kind := range []string{"image", "video"} {
		t.Run(kind, func(t *testing.T) {
			h, key := newVideoPricingHandlerForTest(nil)
			if kind == "image" {
				h, key = newImagePricingHandlerForTest(nil)
			}
			model, tier, mode := "gpt-image-2", "1K", service.BillingModeImage
			if kind == "video" {
				model, tier, mode = "minimax-h3", "720p", service.BillingModeVideo
			}
			billing := h.openAIGatewayService.BillingService()
			billing.SetBasePriceRepository(&guardPriceRepo{price: &service.ModelBasePrice{Model: model, Version: 1, Price: service.AccountModelPrice{Mode: mode, Items: []service.ModelPriceItem{{Kind: kind, Tier: tier, Amount: "0.12"}}}}})
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectExec("INSERT INTO sep_quotes").WillReturnResult(sqlmock.NewResult(1, 1))
			sep := service.NewSEPService(db, service.NewSettingService(sepReceiptSettings{}, &config.Config{}), nil, &service.APIKeyService{}, billing, nil)
			key.SEP = &service.SEPExecution{EnterprisePK: 1, EnterpriseID: "test-enterprise", Kind: kind, Service: sep}
			var data []byte
			if kind == "image" {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/sub2api/images/pricing", strings.NewReader(`{"model":"gpt-image-2","operation":"generate","size":"1024x1024","quality":"medium","n":1}`))
				c.Set(string(middleware2.ContextKeyAPIKey), key)
				h.ImagePricing(c)
				require.Equal(t, 200, w.Code, w.Body.String())
				data = w.Body.Bytes()
			} else {
				w := videoBillingHTTP(t, h, key, "POST", "/v1/sub2api/videos/pricing", validVideoPricingJSON)
				require.Equal(t, 200, w.Code, w.Body.String())
				data = w.Body.Bytes()
			}
			var receipt map[string]any
			require.NoError(t, json.Unmarshal(data, &receipt))
			require.EqualValues(t, 2, receipt["schema_version"])
			require.Equal(t, "CNY", receipt["currency"])
			require.NotEmpty(t, receipt["quote_id"])
			require.NotEmpty(t, receipt["amount_cny"])
			require.Equal(t, "pricing", receipt["receipt_operation"])
			require.Equal(t, "not_applicable", receipt["submission_status"])
			require.Equal(t, "not_applicable", receipt["billing"].(map[string]any)["status"])
			for _, field := range []string{"request_id", "task_id", "is_final", "error", "task_error", "operation_error", "submission_error"} {
				require.Contains(t, receipt, field)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
