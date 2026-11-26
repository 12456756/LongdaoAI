package routes

import (
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
)

func RegisterSEPRoutes(v1 *gin.RouterGroup, h *handler.SEPHandler) {
	if h == nil {
		return
	}
	g := v1.Group("/sep", h.Authenticate)
	g.POST("/enterprises", h.Register)
	g.POST("/enterprises/:enterpriseId/password", h.UpdatePassword)
	g.POST("/enterprises/:enterpriseId/credential-rotations", h.RotateCredential)
	g.GET("/enterprises/:enterpriseId/credential-rotations/:operationId", h.Rotation)
	g.POST("/enterprises/:enterpriseId/credential-rotations/:operationId/:action", h.AdvanceRotation)
	g.GET("/enterprises/:enterpriseId", h.Enterprise)
	g.GET("/enterprises/:enterpriseId/balance", h.Balance)
	g.GET("/enterprises/:enterpriseId/ledger", h.Ledger)
	g.GET("/enterprises/:enterpriseId/balance-events", h.BalanceEvents)
	g.POST("/balance-events/:eventId/ack", h.AcknowledgeBalanceEvent)
	g.GET("/balance-events/signing-key", h.EventSigningKey)
	g.GET("/executions/:executionId/billing", h.Billing)
	g.POST("/executions/:executionId/settlement", h.Settle)
	g.POST("/transfers", h.Transfer)
	g.GET("/transfers/:transferId", h.TransferResult)
	g.POST("/validation/:kind/pricing", h.ValidateMedia)
}
