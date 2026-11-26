package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
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
	if diagnostic != "" {
		body["diagnostic"] = diagnostic
		logger.L().Warn("sep.request_failed", zap.String("request_id", requestID), zap.String("stage", se.Stage), zap.String("code", se.Code), zap.String("diagnostic", diagnostic))
	}
	c.AbortWithStatusJSON(se.Status, body)
}
