package middleware

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func writeSEPAuthError(c *gin.Context, err error) {
	WriteSEPError(c, err, "receive")
}

func WriteSEPError(c *gin.Context, err error, stage string) {
	se, diagnostic := service.NormalizeSEPError(err, stage)
	requestID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	c.Header("X-Request-ID", requestID)
	c.Header("Cache-Control", "no-store")
	executionID := c.Param("executionId")
	if executionID == "" {
		executionID = c.GetHeader("X-SEP-Execution-ID")
	}
	body := gin.H{"error": se, "message": se.Message, "reason": se.Code, "request_id": requestID, "enterprise_id": c.GetHeader("X-SEP-Enterprise-ID"), "execution_id": executionID, "idempotency_key": c.GetHeader("Idempotency-Key")}
	if kind, _ := service.MediaEndpoint(c); kind != "" {
		clientID, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
		body["request_id"], body["platform_request_id"] = clientID, requestID
	}
	var rejected *service.SEPRejectedMediaError
	if errors.As(err, &rejected) && rejected.Key != nil && rejected.Key.SEP != nil {
		// Keep a rejection-only identity separate from authenticated model access.
		c.Set("sep_rejected_media_key", rejected.Key)
		c.Header("X-Compute-Request-ID", rejected.Key.SEP.ComputeRequestID)
		if rejected.BillingError != nil {
			body["enterprise_billing_error"] = gin.H{"source": "platform", "code": "sep_billing_finalize_error", "message": errordetail.Text(rejected.BillingError.Error())}
		}
	}
	if diagnostic != "" {
		body["diagnostic"] = diagnostic
		logger.L().Warn("sep.request_failed", zap.String("request_id", requestID), zap.String("stage", se.Stage), zap.String("code", se.Code), zap.String("diagnostic", diagnostic))
	}
	if kind, _ := service.MediaEndpoint(c); kind != "" {
		service.WriteMediaJSON(c, se.Status, body)
		c.Abort()
		return
	}
	c.AbortWithStatusJSON(se.Status, body)
}

func GetSEPRejectedMediaKey(c *gin.Context) (*service.APIKey, bool) {
	value, ok := c.Get("sep_rejected_media_key")
	key, valid := value.(*service.APIKey)
	return key, ok && valid && key != nil
}
