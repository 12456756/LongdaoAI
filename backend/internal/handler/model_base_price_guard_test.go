package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type guardPriceRepo struct {
	service.ModelBasePriceRepository
	price *service.ModelBasePrice
	err   error
	model string
	saved int
}

func (r *guardPriceRepo) GetBase(_ context.Context, model string) (*service.ModelBasePrice, error) {
	r.model = model
	return r.price, r.err
}

func (r *guardPriceRepo) RememberSnapshot(_ context.Context, _, _ int64, _ string, p *service.ModelBasePrice) (*service.ModelBasePrice, error) {
	r.saved++
	return p, r.err
}

func guardRouter(repo *guardPriceRepo, next gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	billing := &service.BillingService{}
	billing.SetBasePriceRepository(repo)
	gateway := service.NewGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, billing, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler := &GatewayHandler{gatewayService: gateway}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, UserID: 2}) })
	router.POST("/*path", handler.ModelBasePriceGuard(), next)
	return router
}

func TestManagedBasePriceGuardRejectsBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		err        error
	}{
		{"missing", "model_base_price_not_configured", nil},
		{"database", "model_pricing_service_unavailable", errors.New("private database details")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &guardPriceRepo{err: tc.err}
			dispatched := false
			router := guardRouter(repo, func(c *gin.Context) { dispatched = true })
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"custom"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, 503, w.Code)
			require.False(t, dispatched)
			require.Equal(t, "rejected", w.Header().Get("X-Model-Dispatch"))
			require.Contains(t, w.Body.String(), tc.code)
			require.NotContains(t, w.Body.String(), "private database")
		})
	}
}

func TestManagedBasePriceGuardPreservesUploadAndSnapshot(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "custom"))
	require.NoError(t, writer.WriteField("size", "1024x1024"))
	file, err := writer.CreateFormFile("image", "reference.webp")
	require.NoError(t, err)
	_, err = file.Write([]byte{0, 1, 2, 255})
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	original := append([]byte(nil), body.Bytes()...)
	p := &service.ModelBasePrice{Model: "custom", Version: 3, Price: service.AccountModelPrice{Mode: service.BillingModeImage, Items: []service.ModelPriceItem{{Kind: "image", Tier: "1K", Unit: "USD/image", Amount: "0"}}}}
	repo := &guardPriceRepo{price: p}
	router := guardRouter(repo, func(c *gin.Context) {
		got, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		require.Equal(t, original, got)
		require.Nil(t, c.Request.MultipartForm)
		require.Equal(t, p, service.BasePriceSnapshot(c.Request.Context()))
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(original))
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, 204, w.Code)
	require.Equal(t, 1, repo.saved)
}

func TestManagedBasePriceGuardChecksDefaultImageModel(t *testing.T) {
	repo := &guardPriceRepo{}
	router := guardRouter(repo, func(c *gin.Context) { t.Fatal("unpriced image dispatched") })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewBufferString(`{"prompt":"test"}`)))
	require.Equal(t, 503, w.Code)
	require.Equal(t, "gpt-image-2", repo.model)
}

func TestManagedBasePriceGuardRejectsMissingGeminiTier(t *testing.T) {
	repo := &guardPriceRepo{price: &service.ModelBasePrice{Model: "custom", Price: service.AccountModelPrice{Mode: service.BillingModeImage, Items: []service.ModelPriceItem{{Kind: "image", Tier: "2K", Unit: "USD/image", Amount: "0.1"}}}}}
	router := guardRouter(repo, func(c *gin.Context) { t.Fatal("missing 4K price dispatched") })
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1beta/models/custom:generateContent", bytes.NewBufferString(`{"generationConfig":{"imageConfig":{"imageSize":"4K"}}}`)))
	require.Equal(t, 503, w.Code)
	require.Contains(t, w.Body.String(), "model_price_item_not_configured")
	require.Equal(t, "custom", repo.model)
}

func TestManagedBasePriceGuardUsesVideoRequestResolution(t *testing.T) {
	for _, size := range []string{"", "1280x720"} {
		repo := &guardPriceRepo{price: &service.ModelBasePrice{Model: "custom", Price: service.AccountModelPrice{Mode: service.BillingModeVideo, Items: []service.ModelPriceItem{{Kind: "video", Tier: "720p", Unit: "USD/second", Amount: "0.1"}}}}}
		router := guardRouter(repo, func(c *gin.Context) { c.Status(204) })
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewBufferString(`{"model":"custom","size":"`+size+`"}`)))
		require.Equal(t, 204, w.Code, w.Body.String())
	}
}
