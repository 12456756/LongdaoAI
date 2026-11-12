package handler

import "github.com/gin-gonic/gin"

func (h *OpenAIGatewayHandler) VideoReference(c *gin.Context) {
	h.gatewayService.ServeVideoReference(c.Writer, c.Request, c.Param("filename"))
}
