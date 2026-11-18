package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func localDiagnosticMessage(operation string, err error, missing string) string {
	if err != nil {
		return operation + ": " + errordetail.Text(err.Error())
	}
	return operation + ": " + missing
}

func markGatewayDataError(c *gin.Context, field string, err error) {
	if c == nil || err == nil {
		return
	}
	message := errordetail.Text(err.Error())
	logger.L().Warn("gateway.partial_data", zap.String("field", field), zap.String("reason", message))
	var diagnostics []gin.H
	if value, exists := c.Get("gateway_data_diagnostics"); exists {
		diagnostics, _ = value.([]gin.H)
	}
	diagnostics = append(diagnostics, gin.H{"field": field, "source": "platform", "message": message})
	c.Set("gateway_data_diagnostics", diagnostics)
	c.Header("X-Data-Degraded", "true")
}

func writeGatewayData(c *gin.Context, body gin.H) {
	if diagnostics, exists := c.Get("gateway_data_diagnostics"); exists {
		body["diagnostics"] = diagnostics
		body["partial"] = true
	}
	c.JSON(http.StatusOK, body)
}
