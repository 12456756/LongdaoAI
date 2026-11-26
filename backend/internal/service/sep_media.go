package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
)

func (s *SEPService) QuoteResponse(ctx context.Context, key *APIKey, response any, usd *string, kind string) (map[string]any, error) {
	var v map[string]any
	if err := json.Unmarshal(sepJSON(response), &v); err != nil {
		return nil, err
	}
	rate, err := s.settings.GetPlatformExchangeRate(ctx)
	if err != nil {
		return nil, err
	}
	v["contract_version"] = 1
	v["currency"] = "CNY"
	v["amount_cny"] = nil
	delete(v, "amount_usd")
	if usd == nil {
		v["pricing_version"] = ""
		return v, nil
	}
	a, err := decimal.NewFromString(*usd)
	if err != nil || a.IsNegative() {
		return nil, errors.New("invalid quote amount")
	}
	r, _ := decimal.NewFromString(rate)
	amount := a.Mul(r).Round(6).StringFixed(6)
	if _, err = ValidateSEPAmount(amount, false); err != nil {
		return nil, err
	}
	model, _ := v["model"].(string)
	price, err := s.billing.BasePrice(ctx, model)
	if err != nil {
		return nil, err
	}
	if source, _ := v["pricing_source"].(string); source != price.String() {
		return nil, sepConflict("QUOTE_PRICE_CHANGED", "报价过程中基础价格发生变化，请重新报价")
	}
	quoteID := sepRandom("quote_")
	native, _ := v["pricing_version"].(string)
	spec := map[string]any{"model": v["model"], "operation": v["operation"]}
	for _, field := range []string{"size", "quality", "n", "resolution", "duration_seconds"} {
		if value, ok := v[field]; ok {
			spec[field] = value
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO sep_quotes(id,integration_id,enterprise_pk,api_key_id,kind,specification,native_version,exchange_rate,amount_cny,price_snapshot,expires_at,quoted_multiplier) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NOW()+INTERVAL '1 minute',$11)`, quoteID, key.SEP.IntegrationID, key.SEP.EnterprisePK, key.ID, kind, string(sepJSON(spec)), native, rate, amount, string(sepJSON(price)), v["effective_rate_multiplier"])
	if err != nil {
		return nil, err
	}
	v["amount_cny"] = amount
	v["pricing_version"] = quoteID
	v["quote_id"] = quoteID
	v["exchange_rate"] = rate
	v["enterprise_id"] = key.SEP.EnterpriseID
	return v, nil
}
func (s *SEPService) VerifyMediaQuote(ctx context.Context, r *http.Request, body []byte, key *APIKey) error {
	var kind, rate, native string
	var specData, priceData []byte
	var multiplier float64
	err := s.db.QueryRowContext(ctx, `SELECT kind,exchange_rate::text,native_version,specification,price_snapshot,quoted_multiplier FROM sep_quotes WHERE id=$1 AND integration_id=$2 AND enterprise_pk=$3 AND api_key_id=$4 AND expires_at>NOW()`, r.Header.Get("X-SEP-Quote-ID"), key.SEP.IntegrationID, key.SEP.EnterprisePK, key.ID).Scan(&kind, &rate, &native, &specData, &priceData, &multiplier)
	if errors.Is(err, sql.ErrNoRows) {
		return sepConflict("QUOTE_EXPIRED_OR_MISSING", "媒体报价不存在或已过期，请重新报价")
	}
	if err != nil {
		return err
	}
	if kind != key.SEP.Kind {
		return sepConflict("QUOTE_MISMATCH", "报价用途与执行请求不一致")
	}
	current := key.SEP.Multipliers.Image
	if kind == "video" {
		current = key.SEP.Multipliers.Video
	}
	if current != multiplier {
		return sepConflict("QUOTE_PRICE_CHANGED", "分组倍率已变化，请重新报价")
	}
	var spec map[string]any
	if err = json.Unmarshal(specData, &spec); err != nil {
		return err
	}
	var price ModelBasePrice
	if err = json.Unmarshal(priceData, &price); err != nil {
		return err
	}
	// A quote reserves a price only for its short validity period; parameters must match.
	params := map[string]any{}
	if gjson.ValidBytes(body) {
		_ = json.Unmarshal(body, &params)
	} else {
		copyReq := r.Clone(ctx)
		copyReq.Body = io.NopCloser(bytes.NewReader(body))
		if err = copyReq.ParseMultipartForm(32 << 20); err != nil {
			return SEPFailure(400, "INVALID_MULTIPART", "媒体请求格式无效", "quote")
		}
		if copyReq.MultipartForm != nil {
			defer copyReq.MultipartForm.RemoveAll()
			for k, values := range copyReq.MultipartForm.Value {
				if len(values) > 0 {
					params[k] = values[0]
				}
			}
		}
	}
	if value, _ := params["model"].(string); value == "" {
		params["model"] = "gpt-image-2"
	}
	if spec["model"] != params["model"] {
		return sepConflict("QUOTE_MISMATCH", "模型与报价不一致")
	}
	if kind == "image" {
		operation := "generate"
		if strings.HasSuffix(r.URL.Path, "/edits") {
			operation = "edit"
		}
		if spec["operation"] != operation {
			return sepConflict("QUOTE_MISMATCH", "图片操作与报价不一致")
		}
		for _, field := range []string{"size", "quality", "n"} {
			expected := spec[field]
			actual := params[field]
			if actual == nil {
				switch field {
				case "n":
					actual = float64(1)
				case "quality":
					actual = "medium"
				case "size":
					actual = "1024x1024"
				}
			}
			if !sepScalarEqual(expected, actual) {
				return sepConflict("QUOTE_MISMATCH", "图片规格与报价不一致："+field)
			}
		}
	} else {
		seconds := params["seconds"]
		if seconds == nil {
			seconds = params["duration"]
		}
		if !sepScalarEqual(spec["duration_seconds"], seconds) {
			return sepConflict("QUOTE_MISMATCH", "视频时长与报价不一致")
		}
		resolution, _ := params["resolution"].(string)
		if resolution == "" {
			resolution, _ = params["size"].(string)
		}
		if VideoRequestBillingResolution(resolution) != spec["resolution"] {
			return sepConflict("QUOTE_MISMATCH", "视频分辨率与报价不一致")
		}
		if r.Header.Get("X-Video-Operation") != spec["operation"] {
			return sepConflict("QUOTE_MISMATCH", "视频操作与报价不一致")
		}
		r.Header.Set("X-Video-Pricing-Version", native)
	}
	key.SEP.Price = &price
	key.SEP.Rate = rate
	return nil
}
func sepScalarEqual(a, b any) bool {
	return strings.Trim(string(sepJSON(a)), "\"") == strings.Trim(string(sepJSON(b)), "\"")
}

type sepPendingUsage struct {
	Command UsageBillingCommand `json:"command"`
	Log     *UsageLog           `json:"log"`
}

func (s *SEPService) SavePendingMedia(ctx context.Context, e *SEPExecution, cmd *UsageBillingCommand, log *UsageLog) (*UsageBillingApplyResult, error) {
	_, err := s.db.ExecContext(ctx, `UPDATE sep_executions SET pending_usage=$1,account_id=$3,execution_status='running',resource_version=resource_version+1,updated_at=NOW() WHERE id=$2 AND fee_status<>'final'`, string(sepJSON(sepPendingUsage{*cmd, log})), e.ComputeRequestID, cmd.AccountID)
	return &UsageBillingApplyResult{Applied: err == nil}, err
}

func (s *SEPService) refreshVideoBilling(ctx context.Context, integration, executionID string) {
	if s.RefreshVideo == nil {
		return
	}
	var task, enterprise string
	var keyID, pk int64
	err := s.db.QueryRowContext(ctx, `SELECT task_id,api_key_id,enterprise_pk,e.enterprise_id FROM sep_executions x JOIN sep_enterprises e ON e.id=x.enterprise_pk WHERE x.integration_id=$1 AND execution_id=$2 AND kind='video' AND fee_status<>'final' AND task_id IS NOT NULL`, integration, executionID).Scan(&task, &keyID, &pk, &enterprise)
	if err != nil {
		return
	}
	key, err := s.keys.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return
	}
	key.SEP = &SEPExecution{EnterprisePK: pk, EnterpriseID: enterprise, IntegrationID: integration, Kind: "video", Service: s}
	refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_ = s.RefreshVideo(refreshCtx, key, task)
}
func (s *SEPService) ObserveVideo(ctx context.Context, key *APIKey, taskID string, body []byte) error {
	if key == nil || key.SEP == nil || taskID == "" {
		return nil
	}
	state := strings.ToLower(gjson.GetBytes(body, "status").String())
	if state == "" {
		state = strings.ToLower(gjson.GetBytes(body, "data.status").String())
	}
	if state != "completed" && state != "succeeded" && state != "success" && state != "failed" && state != "cancelled" {
		return nil
	}
	var e SEPExecution
	var priceData, multiplierData, pendingData []byte
	var origin string
	err := s.db.QueryRowContext(ctx, `SELECT id,execution_id,model,exchange_rate::text,price_snapshot,multiplier_snapshot,pending_usage,billing_origin FROM sep_executions WHERE enterprise_pk=$1 AND task_id=$2 AND fee_status<>'final'`, key.SEP.EnterprisePK, taskID).Scan(&e.ComputeRequestID, &e.ExecutionID, &e.Model, &e.Rate, &priceData, &multiplierData, &pendingData, &origin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "failed" || state == "cancelled" {
		_, err = s.db.ExecContext(ctx, `UPDATE sep_executions SET execution_status=$1,fee_status='reconciling',resource_version=resource_version+1,updated_at=NOW() WHERE id=$2 AND fee_status<>'final'`, state, e.ComputeRequestID)
		return err
	}
	if len(pendingData) == 0 {
		return nil
	}
	e.Kind = "video"
	e.DirectBilling = origin == "compute"
	e.EnterpriseID = key.SEP.EnterpriseID
	e.IntegrationID = key.SEP.IntegrationID
	e.MediaFinal = true
	e.EnterprisePK = key.SEP.EnterprisePK
	e.Service = s
	var price ModelBasePrice
	var pending sepPendingUsage
	if err = json.Unmarshal(priceData, &price); err != nil {
		return err
	}
	e.Price = &price
	if err = json.Unmarshal(multiplierData, &e.Multipliers); err != nil {
		return err
	}
	if err = json.Unmarshal(pendingData, &pending); err != nil {
		return err
	}
	if price.Price.Mode == BillingModeToken {
		if !gjson.GetBytes(body, "usage").Exists() {
			_, err = s.db.ExecContext(ctx, `UPDATE sep_executions SET execution_status='completed',fee_status='reconciling',updated_at=NOW(),resource_version=resource_version+1 WHERE id=$1 AND fee_status<>'final'`, e.ComputeRequestID)
			return err
		}
		u := extractOpenAIEmbeddingsUsage(body)
		tokens := UsageTokens{InputTokens: u.InputTokens - u.CacheReadInputTokens - u.CacheCreationInputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadInputTokens, CacheCreationTokens: u.CacheCreationInputTokens, ImageInputTokens: u.ImageInputTokens, ImageOutputTokens: u.ImageOutputTokens}
		if tokens.InputTokens < 0 {
			tokens.InputTokens = 0
		}
		cost, err := s.billing.CalculateBasePrice(WithBasePriceSnapshot(ctx, &price), e.Model, "", tokens, 1, 0, e.Multipliers.Token, "")
		if err != nil {
			return err
		}
		pending.Command.BalanceCost = cost.ActualCost
		pending.Command.InputTokens = tokens.InputTokens
		pending.Command.OutputTokens = tokens.OutputTokens
		pending.Command.CacheReadTokens = tokens.CacheReadTokens
		pending.Command.CacheCreationTokens = tokens.CacheCreationTokens
		if pending.Log != nil {
			pending.Log.ActualCost = cost.ActualCost
			pending.Log.TotalCost = cost.TotalCost
		}
	}
	if pending.Log != nil && price.Price.Mode != BillingModeToken {
		seconds := 0
		if pending.Log.VideoDurationSeconds != nil {
			seconds = *pending.Log.VideoDurationSeconds
		}
		if reported := gjson.GetBytes(body, "seconds").Int(); reported > 0 && reported <= 600 {
			seconds = int(reported)
		}
		tier := "720p"
		if pending.Log.VideoResolution != nil {
			tier = *pending.Log.VideoResolution
		}
		if reported := gjson.GetBytes(body, "resolution").String(); reported != "" {
			tier = VideoRequestBillingResolution(reported)
		}
		cost, err := s.billing.CalculateBasePrice(WithBasePriceSnapshot(ctx, &price), e.Model, tier, UsageTokens{}, 1, seconds, e.Multipliers.Video, "")
		if err != nil {
			return err
		}
		pending.Command.BalanceCost = cost.ActualCost
		pending.Log.ActualCost = cost.ActualCost
		pending.Log.TotalCost = cost.TotalCost
		if pending.Command.AccountQuotaCost > 0 {
			rate := 1.0
			if pending.Log.AccountRateMultiplier != nil {
				rate = *pending.Log.AccountRateMultiplier
			}
			pending.Command.AccountQuotaCost = cost.TotalCost * rate
		}
		pending.Log.VideoDurationSeconds = &seconds
		pending.Log.VideoResolution = &tier
	}
	_, err = s.FinalizeUsage(ctx, &e, &pending.Command, pending.Log)
	return err
}
