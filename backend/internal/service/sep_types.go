package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/shopspring/decimal"
)

const SEPSettingKey = "sep_integration_v1"

type SEPConfig struct {
	Enabled              bool   `json:"enabled"`
	IntegrationID        string `json:"integration_id"`
	SigningKeyID         string `json:"signing_key_id"`
	SigningPublicKey     string `json:"signing_public_key"`
	EncryptionKeyID      string `json:"encryption_key_id"`
	EncryptionPublicKey  string `json:"encryption_public_key"`
	ChatGroupID          int64  `json:"chat_group_id"`
	ImageGroupID         int64  `json:"image_group_id"`
	VideoGroupID         int64  `json:"video_group_id"`
	BalanceWebhookURL    string `json:"balance_webhook_url"`
	DirectBillingEnabled bool   `json:"direct_billing_enabled"`
}

type SEPError struct {
	Status           int    `json:"-"`
	Code             string `json:"code"`
	Message          string `json:"message"`
	Stage            string `json:"stage"`
	Retryable        bool   `json:"retryable"`
	NextAction       string `json:"next_action"`
	AccountingStatus string `json:"accounting_status,omitempty"`
	BusinessRef      string `json:"business_ref,omitempty"`
}

func (e *SEPError) Error() string { return e.Code + ": " + e.Message }
func SEPFailure(status int, code, message, stage string) error {
	next := "correct_request"
	if status == 401 {
		next = "verify_service_signature"
	}
	if code == "BILL_NOT_FINAL" {
		next = "query_original_execution"
	}
	if code == "EXECUTION_ALREADY_RECEIVED" {
		next = "query_original_execution"
	}
	if code == "INSUFFICIENT_PAID_FUNDS" {
		next = "reconcile_balance_then_retry_same_settlement"
	}
	if code == "ENTERPRISE_BILL_OUTSTANDING" {
		return &SEPError{Status: status, Code: code, Message: message, Stage: stage, NextAction: "fund_compute_then_reconcile_original_bill", Retryable: true, AccountingStatus: "not_committed"}
	}
	return &SEPError{Status: status, Code: code, Message: message, Stage: stage, NextAction: next, Retryable: code == "BILL_NOT_FINAL" || code == "INSUFFICIENT_PAID_FUNDS"}
}
func sepConflict(code, message string) error { return SEPFailure(409, code, message, "validate") }
func sepMissing() error {
	return SEPFailure(404, "SEP_RESOURCE_NOT_FOUND", "未找到当前集成身份下的资源", "lookup")
}

var sepIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var sepAmountPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,17})\.[0-9]{6}$`)

func ValidateSEPAmount(raw string, positive bool) (decimal.Decimal, error) {
	v, err := decimal.NewFromString(raw)
	if !sepAmountPattern.MatchString(raw) || err != nil || v.IsNegative() || (positive && !v.IsPositive()) {
		return decimal.Zero, SEPFailure(400, "INVALID_CNY_AMOUNT", "人民币金额必须是固定六位小数的非负字符串", "validate")
	}
	return v, nil
}
func sepID(raw string) error {
	if !sepIDPattern.MatchString(raw) {
		return SEPFailure(400, "INVALID_BUSINESS_ID", "业务编号格式无效", "validate")
	}
	return nil
}
func sepRandom(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func sepHash(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }
func sepJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type SEPEnterprise struct {
	ID               int64           `json:"-"`
	IntegrationID    string          `json:"-"`
	EnterpriseID     string          `json:"enterprise_id"`
	ComputeAccountID int64           `json:"compute_account_id"`
	CompanyName      string          `json:"company_name"`
	AdminEmail       string          `json:"admin_email"`
	Status           string          `json:"status"`
	BillingMode      string          `json:"billing_mode"`
	Credentials      []SEPCredential `json:"credentials"`
}
type SEPCredential struct {
	Type            string `json:"type"`
	CredentialID    string `json:"credential_id"`
	Version         int    `json:"credential_version"`
	GroupID         int64  `json:"group_id"`
	EncryptionKeyID string `json:"encryption_key_id"`
	MaskedKey       string `json:"masked_key"`
	EncryptedKey    string `json:"encrypted_key,omitempty"`
	DeliveryStatus  string `json:"delivery_status"`
}
type SEPRegistration struct {
	EnterpriseID    string `json:"enterprise_id"`
	RegistrationRef string `json:"registration_ref"`
	CompanyName     string `json:"company_name"`
	AdminEmail      string `json:"admin_email"`
	EncryptionKeyID string `json:"encryption_key_id"`
	PasswordHash    string `json:"password_hash,omitempty"`
}
type SEPTransfer struct {
	ContractVersion int    `json:"contract_version"`
	EnterpriseID    string `json:"enterprise_id"`
	TransferID      string `json:"transfer_id"`
	Direction       string `json:"direction"`
	Currency        string `json:"currency"`
	AmountCNY       string `json:"amount_cny"`
	FundingRef      string `json:"funding_ref"`
}
type SEPSettlement struct {
	ContractVersion int    `json:"contract_version"`
	EnterpriseID    string `json:"enterprise_id"`
	SettlementID    string `json:"settlement_id"`
	BillID          string `json:"bill_id"`
	BillVersion     int    `json:"bill_version"`
	Currency        string `json:"currency"`
	GrossCNY        string `json:"gross_cny"`
	FreeCNY         string `json:"free_cny"`
	PaidCNY         string `json:"paid_cny"`
}
type SEPMultipliers struct {
	Token float64 `json:"token"`
	Image float64 `json:"image"`
	Video float64 `json:"video"`
}
type SEPExecution struct {
	ComputeRequestID      string          `json:"compute_request_id"`
	ExecutionID           string          `json:"execution_id"`
	EnterpriseID          string          `json:"enterprise_id"`
	IntegrationID         string          `json:"-"`
	EnterprisePK          int64           `json:"-"`
	Kind                  string          `json:"kind"`
	Model                 string          `json:"model_id"`
	Rate                  string          `json:"exchange_rate"`
	Price                 *ModelBasePrice `json:"price_snapshot"`
	Multipliers           SEPMultipliers  `json:"multiplier_snapshot"`
	Service               *SEPService     `json:"-"`
	MediaFinal            bool            `json:"-"`
	MediaBillingRequestID string          `json:"-"`
	MediaTerminalStatus   string          `json:"-"`
	DirectBilling         bool            `json:"-"`
}
type sepContextKey struct{}

func WithSEPExecution(ctx context.Context, e *SEPExecution) context.Context {
	return context.WithValue(ctx, sepContextKey{}, e)
}
func SEPExecutionFromContext(ctx context.Context) *SEPExecution {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(sepContextKey{}).(*SEPExecution)
	return e
}
func SEPRequestID(key *APIKey, fallback string) string {
	if key != nil && key.SEP != nil && key.SEP.ComputeRequestID != "" {
		return key.SEP.ComputeRequestID
	}
	return fallback
}
func SEPUsageContext(ctx context.Context, key *APIKey) context.Context {
	if key != nil && key.SEP != nil {
		ctx = WithSEPExecution(ctx, key.SEP)
		if key.SEP.Price != nil {
			ctx = WithBasePriceSnapshot(ctx, key.SEP.Price)
		}
	}
	return ctx
}
func SEPRequestMultipliers(key *APIKey, token, image, video float64) (float64, float64, float64) {
	if key != nil && key.SEP != nil && key.SEP.ComputeRequestID != "" {
		m := key.SEP.Multipliers
		return m.Token, m.Image, m.Video
	}
	return token, image, video
}
func SEPCanonicalRequest(r *http.Request, body []byte) string {
	return fmt.Sprintf("sep-v1\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s", r.Header.Get("X-SEP-Key-ID"), r.Header.Get("X-SEP-Timestamp"), r.Header.Get("X-SEP-Nonce"), r.Method, r.URL.RequestURI(), sepHash(body), sepHash([]byte(r.Header.Get("Authorization"))), r.Header.Get("X-SEP-Enterprise-ID"), r.Header.Get("X-SEP-Execution-ID"), r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type"), r.Header.Get("X-SEP-Quote-ID"), r.Header.Get("X-Video-Operation"))
}

type SEPBalance struct {
	EnterpriseID string    `json:"enterprise_id"`
	Currency     string    `json:"currency"`
	Total        string    `json:"total_paid_cny"`
	Available    string    `json:"available_paid_cny"`
	Frozen       string    `json:"frozen_paid_cny"`
	Sequence     string    `json:"sequence"`
	ObservedAt   time.Time `json:"observed_at"`
}
