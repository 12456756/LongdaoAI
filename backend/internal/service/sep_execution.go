package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
)

// AuthorizeGateway always reads the binding from durable storage, including when
// API-key authentication was served from cache. Request-specific values use a copy.
func (s *SEPService) AuthorizeGateway(ctx context.Context, r *http.Request, key *APIKey) (*APIKey, error) {
	var enterprisePK, userID int64
	var integration, enterprise, kind, status string
	err := s.db.QueryRowContext(ctx, `SELECT e.id,e.user_id,e.integration_id,e.enterprise_id,COALESCE(c.kind,''),e.status FROM sep_enterprises e LEFT JOIN sep_credentials c ON c.enterprise_pk=e.id AND c.api_key_id=$1 WHERE e.user_id=$2`, key.ID, key.UserID).Scan(&enterprisePK, &userID, &integration, &enterprise, &kind, &status)
	if errors.Is(err, sql.ErrNoRows) {
		if strings.HasPrefix(key.Key, "sk-sep-validation-") {
			return nil, SEPFailure(403, "VALIDATION_CREDENTIAL_INTERNAL_ONLY", "平台校验凭证不能发起模型请求", "authorize")
		}
		if r.Header.Get("X-SEP-Enterprise-ID") != "" {
			return nil, SEPFailure(403, "SEP_KEY_REQUIRED", "该 Key 不属于 SEP 企业", "authorize")
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	if kind == "" {
		return nil, SEPFailure(403, "SEP_KEY_REQUIRED", "请使用平台交付的企业专用 Key", "authorize")
	}
	if r.Header.Get("Authorization") != "Bearer "+key.Key {
		return nil, SEPFailure(401, "SEP_BEARER_KEY_REQUIRED", "企业请求必须使用服务签名覆盖的 Authorization Bearer Key", "authorize")
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(r.Body)
	}
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	direct := r.Header.Get("X-Compute-Billing-Mode") == "direct"
	var cfg *SEPConfig
	if direct {
		if r.Header.Get("X-SEP-Key-ID") != "" || r.Header.Get("X-SEP-Enterprise-ID") != "" || r.Header.Get("X-SEP-Execution-ID") != "" || r.Header.Get("X-SEP-Signature") != "" {
			return nil, SEPFailure(400, "BILLING_ORIGIN_CONFLICT", "直接计费不能同时携带 SEP 服务身份或执行编号", "authorize")
		}
		cfg, err = s.Config(ctx)
		if err == nil && !cfg.Enabled {
			return nil, SEPFailure(403, "SEP_DISABLED", "SEP 企业对接已停用，不能发起独立消费", "authorize")
		}
		if err == nil && !cfg.DirectBillingEnabled {
			return nil, SEPFailure(403, "DIRECT_BILLING_DISABLED", "算力平台独立消费尚未启用，请管理员在 SEP 企业对接中启用", "authorize")
		}
	} else {
		cfg, err = s.Authenticate(ctx, r, body)
	}
	if err != nil {
		return nil, err
	}
	if cfg.IntegrationID != integration || (!direct && r.Header.Get("X-SEP-Enterprise-ID") != enterprise) {
		return nil, SEPFailure(403, "ENTERPRISE_KEY_MISMATCH", "服务身份、企业编号和 Key 归属不匹配", "authorize")
	}
	path := r.URL.Path
	read := r.Method == http.MethodGet && (path == "/v1/models" || path == "/v1/sub2api/billing" || path == "/v1/sub2api/usage" || path == "/v1/sub2api/videos/usage" || strings.HasPrefix(path, "/v1/videos/") || strings.HasPrefix(path, "/v1/images/tasks/"))
	quote := r.Method == http.MethodPost && (path == "/v1/sub2api/images/pricing" || path == "/v1/sub2api/videos/pricing")
	execute := r.Method == http.MethodPost && (path == "/v1/chat/completions" || path == "/v1/responses" || path == "/v1/messages" || path == "/v1/images/generations" || path == "/v1/images/edits" || path == "/v1/images/generations/async" || path == "/v1/images/edits/async" || path == "/v1/videos")
	mediaOperation := sepGatewayMediaOperation(r.Method, path)
	if !read && !quote && !execute && !mediaOperation {
		return nil, SEPFailure(403, "SEP_ENDPOINT_NOT_ALLOWED", "该接口未开放 SEP 专用结算", "authorize")
	}
	requestedKind := "chat"
	if strings.Contains(path, "/images/") {
		requestedKind = "image"
	}
	if strings.Contains(path, "/videos") {
		requestedKind = "video"
	}
	if path != "/v1/models" && path != "/v1/sub2api/billing" && requestedKind != kind {
		return nil, SEPFailure(403, "KEY_PURPOSE_MISMATCH", "Key 用途与调用接口不匹配", "authorize")
	}
	copyKey := *key
	copyKey.SEP = &SEPExecution{EnterprisePK: enterprisePK, EnterpriseID: enterprise, IntegrationID: integration, Kind: kind, Service: s, DirectBilling: direct}
	hash := sepHash([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + r.Header.Get("Content-Type") + "\n" + r.Header.Get("X-SEP-Quote-ID") + "\n" + r.Header.Get("X-Video-Operation") + "\n" + sepHash(body)))
	if execute && kind != "chat" {
		id := r.Header.Get("X-SEP-Execution-ID")
		if direct {
			id = r.Header.Get("Idempotency-Key")
		}
		if err = sepID(id); err != nil {
			return nil, err
		}
		if replay, replayErr := s.replayMediaExecution(ctx, &copyKey, r, id, hash); replayErr != nil || replay != nil {
			return replay, replayErr
		}
	}
	if status != "ready" && !read {
		return nil, SEPFailure(403, "ENTERPRISE_DISABLED", "企业已停用，不能发起新任务", "authorize")
	}
	if key.IsExpired() && !read {
		return nil, SEPFailure(403, "API_KEY_EXPIRED", "企业 Key 已过期", "authorize")
	}
	if err = checkSEPRotatedCredential(ctx, s.db, &copyKey, path, execute || mediaOperation, quote, r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/videos/") && r.URL.Query().Get("id_type") == "request_id"); err != nil {
		return nil, err
	}
	if execute {
		if s.billing != nil && s.billing.cfg != nil && s.billing.cfg.RunMode == config.RunModeSimple {
			return nil, SEPFailure(503, "SEP_STANDARD_MODE_REQUIRED", "SEP 结算需要启用平台标准计费运行模式", "receive")
		}
		if key.Group == nil || key.GroupID == nil {
			return nil, SEPFailure(403, "SEP_GROUP_REQUIRED", "企业 Key 分组不可用", "authorize")
		}
		if cfg == nil || s.billing == nil || !s.billing.UsesManagedBasePrices() {
			return nil, SEPFailure(503, "SEP_PRICING_UNAVAILABLE", "SEP 模型基础价格服务不可用", "receive")
		}
		id := r.Header.Get("X-SEP-Execution-ID")
		if direct {
			id = r.Header.Get("Idempotency-Key")
			var available string
			if err = s.db.QueryRowContext(ctx, `SELECT available_cny::text FROM sep_enterprises WHERE id=$1`, enterprisePK).Scan(&available); err != nil {
				return nil, err
			}
			funds, _ := decimal.NewFromString(available)
			if !funds.IsPositive() {
				return nil, SEPFailure(402, "INSUFFICIENT_PAID_FUNDS", "企业人民币余额为零，不能发起算力平台独立消费；请先充值", "authorize")
			}
			var unpaid bool
			if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_executions x WHERE enterprise_pk=$1 AND billing_origin='compute' AND fee_status='final' AND NOT EXISTS(SELECT 1 FROM sep_settlements t WHERE t.execution_pk=x.id))`, enterprisePK).Scan(&unpaid); err != nil {
				return nil, err
			}
			if unpaid {
				return nil, SEPFailure(402, "DIRECT_BILL_OUTSTANDING", "企业存在尚未扣清的算力平台消费账单，请充值并等待原账单结清后再调用；不会重新生成原任务", "authorize")
			}
		}
		if err = sepID(id); err != nil {
			return nil, err
		}
		var outstanding bool
		if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_pending_settlements WHERE enterprise_pk=$1)
		OR EXISTS(SELECT 1 FROM sep_executions x WHERE enterprise_pk=$1 AND billing_origin='compute' AND fee_status='final'
		AND gross_cny>0 AND NOT EXISTS(SELECT 1 FROM sep_settlements t WHERE t.execution_pk=x.id))`, enterprisePK).Scan(&outstanding); err != nil {
			return nil, err
		}
		if outstanding {
			return nil, SEPFailure(402, "ENTERPRISE_BILL_OUTSTANDING", "企业存在未结清费用，新增生成暂缓；请补足资金并等待原单结清，不要重新生成原任务", "authorize")
		}
		var oldHash string
		var oldPK int64
		lookup := s.db.QueryRowContext(ctx, `SELECT request_hash,enterprise_pk FROM sep_executions WHERE integration_id=$1 AND execution_id=$2`, integration, id).Scan(&oldHash, &oldPK)
		if lookup == nil {
			if kind != "chat" {
				return s.replayMediaExecution(ctx, &copyKey, r, id, hash)
			}
			if oldHash != hash || oldPK != enterprisePK {
				return nil, sepConflict("IDEMPOTENCY_CONFLICT", "执行编号已用于其他请求")
			}
			return nil, sepConflict("EXECUTION_ALREADY_RECEIVED", "请求已受理，请查询原执行费用，不能再次生成")
		}
		if !errors.Is(lookup, sql.ErrNoRows) {
			return nil, lookup
		}
		model, err := sepRequestModel(r, body)
		if err != nil {
			return nil, err
		}
		price, err := s.billing.BasePrice(ctx, model)
		if err != nil {
			return nil, SEPFailure(503, "MODEL_PRICE_UNAVAILABLE", err.Error(), "receive")
		}
		if kind == "chat" && (price.Price.Mode == BillingModeImage || price.Price.Mode == BillingModeVideo || isOpenAIImageModel(model) || sepHasImageOutputTool(body)) {
			return nil, SEPFailure(403, "KEY_PURPOSE_MISMATCH", "对话 Key 不能发起图片或视频生成", "authorize")
		}
		rate, err := s.settings.GetPlatformExchangeRate(ctx)
		if err != nil {
			return nil, err
		}
		base := key.Group.RateMultiplier
		if s.keys.userGroupRateRepo != nil {
			v, e := s.keys.userGroupRateRepo.GetByUserAndGroup(ctx, key.UserID, *key.GroupID)
			if e != nil {
				return nil, e
			}
			if v != nil {
				base = *v
			}
		}
		token, image := computePeakAwareMultipliers(key, base, time.Now())
		video := resolveVideoRateMultiplier(key, base)
		e := copyKey.SEP
		e.ComputeRequestID = sepRandom("compute_")
		e.ExecutionID = id
		e.Model = model
		e.Rate = rate
		e.Price = price
		e.Multipliers = SEPMultipliers{Token: token, Image: image, Video: video}
		var quoteErr error
		if kind != "chat" {
			quoteErr = s.VerifyMediaQuote(ctx, r, body, &copyKey)
		}
		origin := "sep"
		if direct {
			origin = "compute"
		}
		// Serialize acceptance with retirement; no network call holds this lock.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		var locked int64
		if err = tx.QueryRowContext(ctx, `SELECT id FROM sep_enterprises WHERE id=$1 AND status='ready' FOR UPDATE`, enterprisePK).Scan(&locked); err != nil {
			return nil, err
		}
		if err = checkSEPRotatedCredential(ctx, tx, &copyKey, path, true, false); err != nil {
			return nil, err
		}
		clientID, idem := sepMediaIdentity(r)
		result, err := tx.ExecContext(ctx, `INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,group_id,billing_origin,client_request_id,idempotency_key,request_path) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT(integration_id,execution_id) DO NOTHING`, e.ComputeRequestID, integration, enterprisePK, id, key.ID, kind, model, hash, e.Rate, string(sepJSON(e.Price)), string(sepJSON(e.Multipliers)), *key.GroupID, origin, clientID, idem, path)
		if err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			if kind != "chat" {
				return s.replayMediaExecution(ctx, &copyKey, r, id, hash)
			}
			var oldHash string
			var oldPK int64
			if err = s.db.QueryRowContext(ctx, `SELECT request_hash,enterprise_pk FROM sep_executions WHERE integration_id=$1 AND execution_id=$2`, integration, id).Scan(&oldHash, &oldPK); err != nil {
				return nil, err
			}
			if oldHash != hash || oldPK != enterprisePK {
				return nil, sepConflict("IDEMPOTENCY_CONFLICT", "执行编号已用于其他请求")
			}
			return nil, sepConflict("EXECUTION_ALREADY_RECEIVED", "请求已受理，请查询原执行费用，不能再次生成")
		}
		if quoteErr != nil {
			// No provider request has been sent. Preserve this execution so SEP
			// can recover its original rejection and confirm the zero-fee bill.
			finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			billingErr := s.finalizeUnsubmittedMedia(finalCtx, &copyKey)
			cancel()
			return nil, &SEPRejectedMediaError{Key: &copyKey, Cause: quoteErr, BillingError: billingErr}
		}
	}
	return &copyKey, nil
}
func sepRequestModel(r *http.Request, body []byte) (string, error) {
	model := ""
	media, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media == "multipart/form-data" {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		for {
			p, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", SEPFailure(400, "INVALID_MULTIPART", "上传请求格式无效", "receive")
			}
			if p.FormName() == "model" && p.FileName() == "" {
				data, _ := io.ReadAll(io.LimitReader(p, 202))
				model = string(data)
			}
			p.Close()
		}
	} else {
		if !gjson.ValidBytes(body) {
			return "", SEPFailure(400, "INVALID_JSON", "模型请求必须是有效 JSON", "receive")
		}
		model = gjson.GetBytes(body, "model").String()
	}
	if model == "" && strings.Contains(r.URL.Path, "/images/") {
		model = "gpt-image-2"
	}
	if model == "" || len(model) > 200 || strings.TrimSpace(model) != model {
		return "", SEPFailure(400, "INVALID_MODEL", "模型名称无效", "receive")
	}
	return model, nil
}
func (s *SEPService) FinishGateway(ctx context.Context, key *APIKey, status int) {
	if key == nil || key.SEP == nil {
		return
	}
	e := key.SEP
	if e.ComputeRequestID == "" || e.ReplayOnly {
		return
	}
	if e.Kind == "video" || e.Kind == "image" {
		if status >= 400 {
			if err := s.finalizeUnsubmittedMedia(ctx, key); err != nil {
				slog.Error("sep.media_rejection_billing_failed", "compute_request_id", e.ComputeRequestID, "reason", safeVideoError(err))
			}
		}
		return
	}
	// Until usage evidence commits, a successful response is not a final fee.
	state := "running"
	if status >= 400 {
		state = "unknown"
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sep_executions SET execution_status=$1,updated_at=NOW(),resource_version=resource_version+1 WHERE id=$2 AND fee_status='pending'`, state, e.ComputeRequestID)
}
func (s *SEPService) FinalizeUsage(ctx context.Context, e *SEPExecution, cmd *UsageBillingCommand, log *UsageLog) (*UsageBillingApplyResult, error) {
	if e == nil || e.ComputeRequestID == "" || cmd == nil {
		return nil, errors.New("SEP execution context missing")
	}
	if e.Kind == "video" && !e.MediaFinal {
		return s.SavePendingMedia(ctx, e, cmd, log)
	}
	if e.MediaTerminalStatus == "" && !sepHasFeeEvidence(e, cmd, log) {
		_, err := s.db.ExecContext(ctx, `UPDATE sep_executions SET execution_status='completed',fee_status='reconciling',updated_at=NOW(),resource_version=resource_version+1 WHERE id=$1 AND fee_status<>'final'`, e.ComputeRequestID)
		if err != nil {
			return nil, err
		}
		return nil, sepConflict("USAGE_EVIDENCE_MISSING", "上游未提供可确认的 Token 用量，实际费用需要核对")
	}
	cost := cmd.BalanceCost
	if cmd.SubscriptionCost > 0 {
		cost = cmd.SubscriptionCost
	}
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return nil, errors.New("invalid SEP fee evidence")
	}
	usd := decimal.NewFromFloat(cost)
	rate, err := decimal.NewFromString(e.Rate)
	if err != nil || usd.IsNegative() {
		return nil, errors.New("invalid SEP final amount")
	}
	gross := usd.Mul(rate).Round(6).StringFixed(6)
	if _, err = ValidateSEPAmount(gross, false); err != nil {
		return nil, err
	}
	if cmd.AccountQuotaCost > 0 && s.upstreamUsage != nil {
		accountCmd := *cmd
		accountCmd.RequestID = "sep-account:" + e.ComputeRequestID
		accountCmd.DeferredImage, accountCmd.DeferredVideo = false, false
		accountCmd.RequestFingerprint = ""
		accountCmd.BalanceCost = 0
		accountCmd.SubscriptionCost = 0
		accountCmd.SubscriptionID = nil
		accountCmd.APIKeyQuotaCost = 0
		accountCmd.APIKeyRateLimitCost = 0
		if _, err = s.upstreamUsage.Apply(ctx, &accountCmd); err != nil {
			return nil, err
		}
	}
	evidence := sepHash(sepJSON(struct {
		Cost    string
		Command *UsageBillingCommand
	}{usd.String(), cmd}))
	// The fee is immutable once committed; customer funding waits for N05.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// All enterprise balance writers acquire the enterprise before the execution.
	var available, frozen string
	var sequence int64
	if e.DirectBilling {
		if err = tx.QueryRowContext(ctx, `SELECT available_cny::text,frozen_cny::text,sequence FROM sep_enterprises WHERE id=$1 FOR UPDATE`, e.EnterprisePK).Scan(&available, &frozen, &sequence); err != nil {
			return nil, err
		}
	}
	if e.MediaRejectionScope != "" {
		var eligible bool
		if err = tx.QueryRowContext(ctx, `SELECT fee_status='pending' AND CASE WHEN $3='not_dispatched' THEN dispatched_at IS NULL ELSE task_id IS NULL END FROM sep_executions WHERE id=$1 AND api_key_id=$2 FOR UPDATE`, e.ComputeRequestID, cmd.APIKeyID, e.MediaRejectionScope).Scan(&eligible); err != nil {
			return nil, err
		}
		if !eligible {
			return &UsageBillingApplyResult{Applied: false}, nil
		}
	}
	var fee, oldEvidence string
	err = tx.QueryRowContext(ctx, `SELECT fee_status,COALESCE(fee_evidence_hash,'') FROM sep_executions WHERE id=$1 AND api_key_id=$2 FOR UPDATE`, e.ComputeRequestID, cmd.APIKeyID).Scan(&fee, &oldEvidence)
	if err != nil {
		return nil, err
	}
	if fee == "final" {
		if oldEvidence != evidence {
			return nil, sepConflict("FINAL_FEE_CONFLICT", "已确认费用证据不一致，需要人工核对")
		}
		return &UsageBillingApplyResult{Applied: false}, nil
	}
	if err = lockSEPMediaOutcome(ctx, tx, e, cmd); err != nil {
		return nil, err
	}
	usage := map[string]any{"input_tokens": cmd.InputTokens, "output_tokens": cmd.OutputTokens, "cache_creation_tokens": cmd.CacheCreationTokens, "cache_read_tokens": cmd.CacheReadTokens, "image_count": cmd.ImageCount}
	if log != nil {
		usage["video_count"] = log.VideoCount
		usage["video_duration_seconds"] = log.VideoDurationSeconds
	}
	_, err = tx.ExecContext(ctx, `UPDATE sep_executions SET execution_status='completed',fee_status='final',billing_error='',gross_usd=$1,gross_cny=$2,bill_id=$3,bill_version=1,fee_evidence_hash=$4,usage=$5,account_id=COALESCE(NULLIF($6,0),account_id),finalized_at=NOW(),updated_at=NOW(),resource_version=resource_version+1 WHERE id=$7`, usd.String(), gross, sepRandom("bill_"), evidence, string(sepJSON(usage)), cmd.AccountID, e.ComputeRequestID)
	if err != nil {
		return nil, err
	}
	if e.MediaTerminalStatus != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE sep_executions SET execution_status=$1 WHERE id=$2`, e.MediaTerminalStatus, e.ComputeRequestID); err != nil {
			return nil, err
		}
	}
	if e.Kind == "video" && log != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE usage_logs SET actual_cost=$1,total_cost=$2,video_duration_seconds=$3,video_resolution=$4 WHERE request_id=$5 AND api_key_id=$6`, cost, log.TotalCost, log.VideoDurationSeconds, log.VideoResolution, e.ComputeRequestID, cmd.APIKeyID); err != nil {
			return nil, err
		}
	}
	if e.DirectBilling {
		if err = settleSEPDirect(ctx, tx, e.ComputeRequestID, e.EnterprisePK, available, frozen, sequence, gross); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &UsageBillingApplyResult{Applied: true}, nil
}

func sepHasFeeEvidence(e *SEPExecution, cmd *UsageBillingCommand, log *UsageLog) bool {
	if e.Price == nil || e.Price.Price.Mode != BillingModeToken {
		return true
	}
	if cmd.InputTokens > 0 || cmd.OutputTokens > 0 || cmd.CacheReadTokens > 0 || cmd.CacheCreationTokens > 0 {
		return true
	}
	return log != nil && (log.ImageInputTokens > 0 || log.ImageOutputTokens > 0 || log.CacheCreation5mTokens > 0 || log.CacheCreation1hTokens > 0)
}

func (s *SEPService) Billing(ctx context.Context, integration, executionID string) (map[string]any, error) {
	s.refreshVideoBilling(ctx, integration, executionID)
	if _, err := s.db.ExecContext(ctx, `UPDATE sep_executions SET execution_status='unknown',fee_status='reconciling',resource_version=resource_version+1 WHERE integration_id=$1 AND execution_id=$2 AND fee_status='pending' AND execution_status IN ('received','running','unknown') AND updated_at<NOW()-INTERVAL '10 minutes'`, integration, executionID); err != nil {
		return nil, err
	}
	var id, enterprise, kind, model, state, fee, rate string
	var version int64
	var priceData, usage, receipt []byte
	var gross, bill, provider, task sql.NullString
	var billVersion sql.NullInt64
	var finalized sql.NullTime
	var updated time.Time
	var origin, billingError string
	err := s.db.QueryRowContext(ctx, `SELECT x.id,e.enterprise_id,x.kind,x.model,x.execution_status,x.fee_status,x.resource_version,x.exchange_rate::text,x.price_snapshot,x.usage,x.gross_cny::text,x.bill_id,x.bill_version,x.provider_request_id,x.task_id,x.finalized_at,x.updated_at,s.receipt,x.billing_origin,x.billing_error FROM sep_executions x JOIN sep_enterprises e ON e.id=x.enterprise_pk LEFT JOIN sep_settlements s ON s.execution_pk=x.id WHERE x.integration_id=$1 AND x.execution_id=$2`, integration, executionID).Scan(&id, &enterprise, &kind, &model, &state, &fee, &version, &rate, &priceData, &usage, &gross, &bill, &billVersion, &provider, &task, &finalized, &updated, &receipt, &origin, &billingError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	var p ModelBasePrice
	if err = json.Unmarshal(priceData, &p); err != nil {
		return nil, err
	}
	var u any
	if len(usage) > 0 {
		_ = json.Unmarshal(usage, &u)
	}
	var rec any
	if len(receipt) > 0 {
		_ = json.Unmarshal(receipt, &rec)
	}
	var amount any
	if gross.Valid {
		amount = gross.String
	}
	var at any
	if finalized.Valid {
		at = finalized.Time
	}
	settlementStatus := "unsettled"
	if rec != nil {
		settlementStatus = "committed"
	}
	return map[string]any{"contract_version": 1, "enterprise_id": enterprise, "execution_id": executionID, "kind": kind, "model_id": model, "compute_request_id": id, "provider_request_id": provider.String, "task_id": task.String, "execution_status": state, "fee_status": fee, "resource_version": strconv.FormatInt(version, 10), "bill_id": bill.String, "bill_version": billVersion.Int64, "price_version": p.String(), "currency": "CNY", "gross_cny": amount, "exchange_rate": rate, "usage": u, "settlement_status": settlementStatus, "settlement_receipt": rec, "finalized_at": at, "billing_origin": origin, "billing_error": billingError}, nil
}
func (s *SEPService) RecordExecutionProvider(ctx context.Context, key *APIKey, provider, task string) error {
	if key == nil || key.SEP == nil || key.SEP.ComputeRequestID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sep_executions SET provider_request_id=COALESCE(NULLIF($1,''),provider_request_id),task_id=COALESCE(NULLIF($2,''),task_id),updated_at=NOW(),resource_version=resource_version+1 WHERE id=$3`, provider, task, key.SEP.ComputeRequestID)
	return err
}
