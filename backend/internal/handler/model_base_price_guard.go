package handler

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Run after API-key authentication and before any upstream dispatch.
func (h *GatewayHandler) ModelBasePriceGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.gatewayService == nil || h.gatewayService.BillingService() == nil || !h.gatewayService.BillingService().UsesManagedBasePrices() || (h.cfg != nil && h.cfg.RunMode == config.RunModeSimple) {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		if c.Request.Method != http.MethodPost || strings.Contains(path, "/sub2api/") || strings.Contains(path, "count_tokens") || strings.Contains(path, "/alpha/search") || strings.HasSuffix(path, "/cancel") {
			c.Next()
			return
		}
		model, tier := "", ""
		if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
			body, readErr := io.ReadAll(c.Request.Body)
			if readErr != nil {
				h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "无法读取上传内容")
				c.Abort()
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
				h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "图片或视频请求表单无法解析")
				c.Abort()
				return
			}
			model = c.Request.FormValue("model")
			tier = c.Request.FormValue("resolution")
			if tier == "" {
				tier = c.Request.FormValue("size")
			}
			if c.Request.MultipartForm != nil {
				c.Request.MultipartForm.RemoveAll()
			}
			c.Request.MultipartForm = nil
			c.Request.Form = nil
			c.Request.PostForm = nil
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		} else if c.Request.Body != nil {
			body, err := io.ReadAll(c.Request.Body)
			if err != nil {
				h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "无法读取请求内容")
				c.Abort()
				return
			}
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			if !gjson.ValidBytes(body) {
				c.Next()
				return
			}
			model = gjson.GetBytes(body, "model").String()
			tier = gjson.GetBytes(body, "resolution").String()
			if tier == "" {
				tier = gjson.GetBytes(body, "size").String()
			}
			if tier == "" {
				for _, field := range []string{"generationConfig.imageConfig.imageSize", "generation_config.image_config.image_size", "image_size"} {
					if value := gjson.GetBytes(body, field).String(); value != "" {
						tier = value
						break
					}
				}
			}
		}
		if model == "" && strings.Contains(path, "/models/") {
			suffix := strings.SplitN(path, "/models/", 2)[1]
			model = strings.SplitN(suffix, ":", 2)[0]
		}
		model = strings.TrimSpace(model)
		imagePath := strings.TrimSuffix(path, "/async")
		if model == "" && (strings.HasSuffix(imagePath, "/images/generations") || strings.HasSuffix(imagePath, "/images/edits")) {
			model = "gpt-image-2"
		}
		if model == "" {
			c.Next()
			return
		}
		billing := h.gatewayService.BillingService()
		price, err := billing.BasePrice(c.Request.Context(), model)
		if err == nil && ((strings.Contains(imagePath, "/images/") && price.Price.Mode == service.BillingModeVideo) || ((strings.Contains(path, "/videos") || strings.Contains(path, "/video/")) && price.Price.Mode == service.BillingModeImage)) {
			err = infraerrors.New(http.StatusServiceUnavailable, "model_price_mode_mismatch", "模型基础价格的计费方式与当前接口不匹配，请联系管理员检查")
		}
		if err == nil {
			kind := ""
			switch price.Price.Mode {
			case service.BillingModeImage:
				kind = "image"
				tier = service.NormalizeImageBillingTierOrDefault(tier)
			case service.BillingModeVideo:
				kind = "video"
				if strings.HasSuffix(path, "/videos") || strings.HasSuffix(path, "/video/generations") {
					tier = service.VideoRequestBillingResolution(tier)
				} else {
					tier = service.NormalizeVideoBillingResolutionOrDefault(tier)
				}
			case service.BillingModePerRequest:
				kind = "request"
			}
			if kind != "" {
				_, ok := price.Rate(kind, tier)
				if !ok && kind == "request" {
					_, ok = price.Rate(kind, "")
				}
				if !ok {
					err = infraerrors.Newf(http.StatusServiceUnavailable, "model_price_item_not_configured", "模型 %s 未配置 %s 的基础价格，本次请求未提交上游", model, tier)
				}
			}
		}
		if err != nil {
			abortModelPricingRequest(c, err)
			return
		}
		if key, ok := middleware.GetAPIKeyFromContext(c); ok {
			snapshot, snapshotErr := billing.RememberBasePrice(c.Request.Context(), key.ID, key.UserID, price)
			if snapshotErr != nil {
				abortModelPricingRequest(c, snapshotErr)
				return
			}
			price = snapshot
		}
		c.Request = c.Request.WithContext(service.WithBasePriceSnapshot(c.Request.Context(), price))
		c.Next()
	}
}

func abortModelPricingRequest(c *gin.Context, err error) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Model-Dispatch", "rejected")
	c.AbortWithStatusJSON(infraerrors.Code(err), gin.H{"error": gin.H{"type": "pricing_error", "code": infraerrors.Reason(err), "message": infraerrors.Message(err), "source": "platform", "dispatch": "not_dispatched", "request_id": c.Writer.Header().Get("X-Request-ID")}})
}

func writeModelPricingError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	reason := infraerrors.Reason(err)
	if !strings.HasPrefix(reason, "model_") {
		return false
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(infraerrors.Code(err), gin.H{"error": gin.H{"type": "pricing_error", "code": reason, "message": infraerrors.Message(err), "source": "platform", "request_id": c.Writer.Header().Get("X-Request-ID")}})
	return true
}
