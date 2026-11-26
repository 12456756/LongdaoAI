package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type SEPHandler struct {
	svc     *service.SEPService
	gateway *GatewayHandler
}

func NewSEPHandler(svc *service.SEPService, gateway *service.OpenAIGatewayService, handlers *GatewayHandler) *SEPHandler {
	svc.RefreshVideo = gateway.RefreshSEPVideo
	svc.StartBalanceSync()
	return &SEPHandler{svc: svc, gateway: handlers}
}

func (h *SEPHandler) ValidateMedia(c *gin.Context) {
	kind := c.Param("kind")
	if kind == "images" {
		kind = "image"
	}
	if kind == "videos" {
		kind = "video"
	}
	key, err := h.svc.ValidationKey(c.Request.Context(), h.config(c), kind)
	if err != nil {
		h.failure(c, err)
		return
	}
	c.Set(string(middleware.ContextKeyAPIKey), key)
	c.Set("sep_platform_validation", h.svc)
	if kind == "image" {
		h.gateway.ImagePricing(c)
	} else {
		h.gateway.VideoPricing(c)
	}
}
func (h *SEPHandler) failure(c *gin.Context, err error) {
	middleware.WriteSEPError(c, err, "storage")
}
func (h *SEPHandler) Authenticate(c *gin.Context) {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1024*1024))
	if err != nil {
		h.failure(c, service.SEPFailure(400, "INVALID_BODY", "请求内容过大或无法读取", "authenticate"))
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	cfg, err := h.svc.Authenticate(c.Request.Context(), c.Request, body)
	if err != nil {
		h.failure(c, err)
		return
	}
	c.Set("sep_config", cfg)
	c.Next()
}
func (h *SEPHandler) decode(c *gin.Context, v any) bool {
	d := json.NewDecoder(c.Request.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		h.failure(c, service.SEPFailure(400, "INVALID_REQUEST", "请求字段或金额格式无效", "validate"))
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		h.failure(c, service.SEPFailure(400, "INVALID_REQUEST", "请求必须是单个 JSON 对象", "validate"))
		return false
	}
	return true
}
func (h *SEPHandler) config(c *gin.Context) *service.SEPConfig {
	return c.MustGet("sep_config").(*service.SEPConfig)
}
func (h *SEPHandler) send(c *gin.Context, value any, err error) {
	if err != nil {
		h.failure(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, value)
}
func (h *SEPHandler) idempotency(c *gin.Context, id string) bool {
	if id == "" || c.GetHeader("Idempotency-Key") != id {
		h.failure(c, service.SEPFailure(400, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key 必须与业务编号一致", "validate"))
		return false
	}
	return true
}
func (h *SEPHandler) Register(c *gin.Context) {
	var r service.SEPRegistration
	if !h.decode(c, &r) || !h.idempotency(c, r.RegistrationRef) {
		return
	}
	v, e := h.svc.Register(c.Request.Context(), h.config(c), r)
	h.send(c, v, e)
}
func (h *SEPHandler) UpdatePassword(c *gin.Context) {
	var r service.SEPPasswordUpdate
	if !h.decode(c, &r) || !h.idempotency(c, r.OperationID) {
		return
	}
	if c.GetHeader("X-SEP-Enterprise-ID") != c.Param("enterpriseId") {
		h.failure(c, service.SEPFailure(400, "INVALID_ENTERPRISE_IDENTITY", "签名企业必须与密码同步企业一致", "password"))
		return
	}
	v, e := h.svc.UpdatePassword(c.Request.Context(), h.config(c).IntegrationID, c.Param("enterpriseId"), r)
	h.send(c, v, e)
}
func (h *SEPHandler) Enterprise(c *gin.Context) {
	v, e := h.svc.Enterprise(c.Request.Context(), h.config(c).IntegrationID, c.Param("enterpriseId"), false)
	h.send(c, v, e)
}

func (h *SEPHandler) RotateCredential(c *gin.Context) {
	if !h.rotationEnterprise(c) {
		return
	}
	var r service.SEPRotationRequest
	if !h.decode(c, &r) || !h.idempotency(c, r.OperationID) {
		return
	}
	v, err := h.svc.RotateCredential(c.Request.Context(), h.config(c), c.Param("enterpriseId"), r)
	h.send(c, v, err)
}
func (h *SEPHandler) Rotation(c *gin.Context) {
	if !h.rotationEnterprise(c) {
		return
	}
	v, err := h.svc.Rotation(c.Request.Context(), h.config(c).IntegrationID, c.Param("enterpriseId"), c.Param("operationId"))
	h.send(c, v, err)
}
func (h *SEPHandler) AdvanceRotation(c *gin.Context) {
	if !h.rotationEnterprise(c) {
		return
	}
	if c.Param("action") != "ack" && c.Param("action") != "retire" {
		h.failure(c, service.SEPFailure(404, "SEP_RESOURCE_NOT_FOUND", "凭证操作不存在", "rotate"))
		return
	}
	if !h.idempotency(c, c.Param("operationId")) {
		return
	}
	v, err := h.svc.AdvanceRotation(c.Request.Context(), h.config(c).IntegrationID, c.Param("enterpriseId"), c.Param("operationId"), c.Param("action") == "retire")
	h.send(c, v, err)
}
func (h *SEPHandler) rotationEnterprise(c *gin.Context) bool {
	if c.GetHeader("X-SEP-Enterprise-ID") != c.Param("enterpriseId") {
		h.failure(c, service.SEPFailure(400, "INVALID_ENTERPRISE_IDENTITY", "签名企业必须与凭证更换企业一致", "rotate"))
		return false
	}
	return true
}
func (h *SEPHandler) Billing(c *gin.Context) {
	v, e := h.svc.Billing(c.Request.Context(), h.config(c).IntegrationID, c.Param("executionId"))
	h.send(c, v, e)
}
func (h *SEPHandler) Settle(c *gin.Context) {
	var r service.SEPSettlement
	if !h.decode(c, &r) || !h.idempotency(c, r.SettlementID) {
		return
	}
	v, e := h.svc.Settle(c.Request.Context(), h.config(c).IntegrationID, c.Param("executionId"), r)
	h.send(c, v, e)
}
func (h *SEPHandler) Transfer(c *gin.Context) {
	var r service.SEPTransfer
	if !h.decode(c, &r) || !h.idempotency(c, r.TransferID) {
		return
	}
	v, e := h.svc.Transfer(c.Request.Context(), h.config(c).IntegrationID, r)
	h.send(c, v, e)
}
func (h *SEPHandler) TransferResult(c *gin.Context) {
	v, e := h.svc.TransferResult(c.Request.Context(), h.config(c).IntegrationID, c.Param("transferId"))
	h.send(c, v, e)
}
func (h *SEPHandler) Balance(c *gin.Context) {
	v, e := h.svc.Balance(c.Request.Context(), h.config(c).IntegrationID, c.Param("enterpriseId"))
	h.send(c, v, e)
}
func (h *SEPHandler) Ledger(c *gin.Context) {
	after, e := strconv.ParseInt(c.DefaultQuery("after_sequence", "0"), 10, 64)
	watermark, e2 := strconv.ParseInt(c.DefaultQuery("snapshot_sequence", "0"), 10, 64)
	limit, e3 := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if e != nil || e2 != nil || e3 != nil {
		h.failure(c, service.SEPFailure(400, "INVALID_CURSOR", "流水查询参数无效", "ledger"))
		return
	}
	v, err := h.svc.Ledger(c.Request.Context(), h.config(c).IntegrationID, c.Param("enterpriseId"), after, watermark, limit)
	h.send(c, v, err)
}
func (h *SEPHandler) AdminConfig(c *gin.Context) {
	if c.Request.Method == http.MethodPut {
		var r service.SEPConfig
		if !h.decode(c, &r) {
			return
		}
		if err := h.svc.SaveConfig(c.Request.Context(), &r); err != nil {
			h.failure(c, err)
			return
		}
	}
	v, e := h.svc.Config(c.Request.Context())
	h.send(c, v, e)
}

func (h *SEPHandler) EventSigningKey(c *gin.Context) { h.send(c, service.SEPEventPublicKey(), nil) }
func (h *SEPHandler) BalanceEvents(c *gin.Context) {
	cfg, err := h.svc.Config(c.Request.Context())
	if err != nil {
		h.failure(c, err)
		return
	}
	after, err := strconv.ParseInt(c.DefaultQuery("after_sequence", "0"), 10, 64)
	limit, e2 := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if err != nil || e2 != nil {
		h.failure(c, service.SEPFailure(400, "INVALID_EVENT_CURSOR", "同步事件查询的游标或条数不是整数", "events"))
		return
	}
	result, err := h.svc.BalanceEvents(c.Request.Context(), cfg.IntegrationID, c.Param("enterpriseId"), after, limit)
	h.send(c, result, err)
}
func (h *SEPHandler) AcknowledgeBalanceEvent(c *gin.Context) {
	if !h.idempotency(c, c.Param("eventId")) {
		return
	}
	err := h.svc.AcknowledgeBalanceEvent(c.Request.Context(), h.config(c).IntegrationID, c.Param("eventId"))
	h.send(c, gin.H{"event_id": c.Param("eventId"), "status": "applied"}, err)
}
func (h *SEPHandler) RetryBalanceEvent(c *gin.Context) {
	cfg, err := h.svc.Config(c.Request.Context())
	if err != nil {
		h.failure(c, err)
		return
	}
	err = h.svc.RetryBalanceEvent(c.Request.Context(), cfg.IntegrationID, c.Param("eventId"))
	h.send(c, gin.H{"event_id": c.Param("eventId"), "status": "pending"}, err)
}
func (h *SEPHandler) AdminEnterprises(c *gin.Context) {
	v, e := h.svc.ListEnterprises(c.Request.Context(), 200)
	h.send(c, v, e)
}
func (h *SEPHandler) AdminEnterprise(c *gin.Context) {
	cfg, e := h.svc.Config(c.Request.Context())
	if e != nil {
		h.failure(c, e)
		return
	}
	id := c.Param("enterpriseId")
	if c.Request.Method == http.MethodPut {
		var r struct {
			Status string           `json:"status"`
			Groups map[string]int64 `json:"groups"`
		}
		if !h.decode(c, &r) {
			return
		}
		if e = h.svc.ManageEnterprise(c.Request.Context(), cfg.IntegrationID, id, r.Status, r.Groups); e != nil {
			h.failure(c, e)
			return
		}
	}
	enterprise, e := h.svc.Enterprise(c.Request.Context(), cfg.IntegrationID, id, false)
	if e != nil {
		h.failure(c, e)
		return
	}
	balance, e := h.svc.Balance(c.Request.Context(), cfg.IntegrationID, id)
	if e != nil {
		h.failure(c, e)
		return
	}
	bills, e := h.svc.EnterpriseBills(c.Request.Context(), cfg.IntegrationID, id)
	h.send(c, gin.H{"enterprise": enterprise, "balance": balance, "bills": bills}, e)
}
func (h *SEPHandler) AdminLedger(c *gin.Context) {
	cfg, e := h.svc.Config(c.Request.Context())
	if e != nil {
		h.failure(c, e)
		return
	}
	after, e := strconv.ParseInt(c.DefaultQuery("after_sequence", "0"), 10, 64)
	watermark, e2 := strconv.ParseInt(c.DefaultQuery("snapshot_sequence", "0"), 10, 64)
	if e != nil || e2 != nil {
		h.failure(c, service.SEPFailure(400, "INVALID_CURSOR", "流水查询参数无效", "ledger"))
		return
	}
	v, e := h.svc.Ledger(c.Request.Context(), cfg.IntegrationID, c.Param("enterpriseId"), after, watermark, 100)
	h.send(c, v, e)
}
