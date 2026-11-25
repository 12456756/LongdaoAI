package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const ImageProtocolBackground = "background_sync"
const ImageProtocolNaoshengyun = "naoshengyun_gpt_async"
const OpenAIImagesCapabilityAsync OpenAIImagesCapability = "images-naoshengyun-async"

func (a *Account) ImageProtocol() string {
	if a != nil && a.GetCredential("image_protocol") != "" {
		return a.GetCredential("image_protocol")
	}
	return ImageProtocolBackground
}

func ValidateImageProtocolCredentials(credentials map[string]any) error {
	if v, ok := credentials["image_protocol"]; ok && v != ImageProtocolBackground && v != ImageProtocolNaoshengyun {
		return infraerrors.BadRequest("INVALID_IMAGE_PROTOCOL", "image_protocol must be background_sync or naoshengyun_gpt_async")
	}
	return nil
}

// No image bytes, prompts or credentials are stored in this task metadata.
type AsyncImageMetadata struct {
	RequestID         string                     `json:"request_id"`
	IdempotencyKey    string                     `json:"idempotency_key"`
	Fingerprint       string                     `json:"fingerprint"`
	SubmissionStatus  string                     `json:"submission_status"`
	AccountID         int64                      `json:"account_id"`
	GroupID           int64                      `json:"group_id"`
	UpstreamTaskID    string                     `json:"upstream_task_id,omitempty"`
	UpstreamStatus    string                     `json:"upstream_status,omitempty"`
	UpstreamRequestID string                     `json:"upstream_request_id,omitempty"`
	UpstreamModel     string                     `json:"upstream_model"`
	ResultURL         string                     `json:"result_url,omitempty"`
	Protocol          string                     `json:"protocol"`
	DispatchDeadline  int64                      `json:"dispatch_deadline"`
	Billing           *AsyncImageBillingSnapshot `json:"billing,omitempty"`
	BillingRegistered bool                       `json:"billing_registered,omitempty"`
}

type ImageTaskBillingStatus struct {
	Status        string  `json:"status"`
	ActualCostUSD *string `json:"actual_cost_usd"`
	Error         any     `json:"error,omitempty"`
}

func imageNotCharged() *ImageTaskBillingStatus {
	zero := "0"
	return &ImageTaskBillingStatus{Status: "not_charged", ActualCostUSD: &zero}
}

// Claim atomically creates both aliases and a record, before any paid dispatch.
// Save must preserve terminal states against stale concurrent query results.
type AsyncImageTaskStore interface {
	ImageTaskStore
	Claim(context.Context, *ImageTaskRecord, time.Duration) (*ImageTaskRecord, bool, error)
	FindClaim(context.Context, int64, string, string) (*ImageTaskRecord, error)
}

func (s *ImageTaskService) FindAsyncClaim(ctx context.Context, key *APIKey, requestID, idem string) (*ImageTaskRecord, error) {
	store, ok := s.store.(AsyncImageTaskStore)
	if !ok {
		return nil, errors.New("Atomic asynchronous image task storage is unavailable")
	}
	record, err := store.FindClaim(ctx, key.ID, requestID, idem)
	if err != nil {
		return nil, err
	}
	if record != nil && (record.UserID != key.UserID || record.APIKeyID != key.ID) {
		return nil, ErrImageTaskNotFound
	}
	return record, nil
}

func (s *ImageTaskService) LoadRecord(ctx context.Context, owner ImageTaskOwner, id string) (*ImageTaskRecord, error) {
	if s == nil || s.store == nil {
		return nil, ErrImageTaskUnavailable
	}
	record, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if record.UserID != owner.UserID || record.APIKeyID != owner.APIKeyID {
		return nil, ErrImageTaskNotFound
	}
	return record, nil
}

func (s *ImageTaskService) ClaimAsync(ctx context.Context, key *APIKey, meta *AsyncImageMetadata) (*ImageTaskRecord, bool, error) {
	store, ok := s.store.(AsyncImageTaskStore)
	if !ok {
		return nil, false, errors.New("Atomic asynchronous image task storage is unavailable")
	}
	now := time.Now().UTC()
	record := &ImageTaskRecord{ID: "imgtask_" + strings.ReplaceAll(uuid.NewString(), "-", ""), UserID: key.UserID, APIKeyID: key.ID, Status: ImageTaskStatusProcessing, CreatedAt: now.Unix(), ExpiresAt: now.Add(s.ttl).Unix(), Provider: meta}
	return store.Claim(ctx, record, s.ttl)
}

func (s *ImageTaskService) SaveAsync(ctx context.Context, record *ImageTaskRecord) error {
	ttl := time.Until(time.Unix(record.ExpiresAt, 0))
	if ttl <= 0 {
		return ErrImageTaskNotFound
	}
	if err := s.store.Save(ctx, record, ttl); err != nil {
		return fmt.Errorf("Save asynchronous image task: %w", err)
	}
	return nil
}

func PublicAsyncImage(record *ImageTaskRecord) *ImageTask { return imageTaskToPublic(record) }

func (s *OpenAIGatewayService) SelectAccountForAsyncImages(ctx context.Context, groupID *int64, model string) (*AccountSelectionResult, error) {
	selection, _, err := s.selectAccountWithScheduler(ctx, groupID, "", "", model, nil, OpenAIUpstreamTransportHTTPSSE, "", OpenAIImagesCapabilityAsync, false, PlatformOpenAI, false, false)
	return selection, err
}

func (s *OpenAIGatewayService) AsyncImageProtocolConfigured(ctx context.Context, groupID *int64, model string) (bool, error) {
	if s.accountRepo == nil || groupID == nil {
		return false, nil
	}
	accounts, err := s.accountRepo.ListByGroup(ctx, *groupID)
	if err != nil {
		return false, fmt.Errorf("Read image protocol configuration: %w", err)
	}
	for _, account := range accounts {
		if account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey && account.ImageProtocol() == ImageProtocolNaoshengyun && account.IsModelSupported(model) {
			return true, nil
		}
	}
	return false, nil
}

func (s *OpenAIGatewayService) AsyncImageTaskAccount(ctx context.Context, key *APIKey, record *ImageTaskRecord) (*Account, error) {
	p := record.Provider
	if p == nil || key.GroupID == nil || p.GroupID != *key.GroupID {
		return nil, ErrImageTaskNotFound
	}
	account, err := s.accountRepo.GetByID(ctx, p.AccountID)
	if err != nil {
		return nil, fmt.Errorf("Read original image task account: %w", err)
	}
	if account == nil || account.Type != AccountTypeAPIKey || account.Platform != PlatformOpenAI || account.ImageProtocol() != ImageProtocolNaoshengyun {
		return nil, errors.New("Original image account is missing or its protocol changed")
	}
	bound := false
	for _, groupID := range account.GroupIDs {
		if groupID == p.GroupID {
			bound = true
			break
		}
	}
	if !bound {
		return nil, errors.New("Original image account is no longer assigned to the task group")
	}
	return account, nil
}

func failAsyncImage(record *ImageTaskRecord, failure *AsyncImageError) {
	now := time.Now().Unix()
	record.Status, record.HTTPStatus, record.CompletedAt = ImageTaskStatusFailed, failure.Status, &now
	if record.Provider.SubmissionStatus != "accepted" {
		record.Provider.SubmissionStatus = "failed"
	}
	record.Error, _ = json.Marshal(failure.Fields)
}

func asyncImageRecordFailure(record *ImageTaskRecord) *AsyncImageError {
	var fields map[string]any
	_ = json.Unmarshal(record.Error, &fields)
	if fields == nil {
		fields = map[string]any{"source": "platform", "code": "image_submission_failed", "message": "Original image submission failed; submit a new request ID to generate again"}
	}
	status := record.HTTPStatus
	if status < 400 {
		status = http.StatusBadGateway
	}
	return &AsyncImageError{Status: status, Fields: fields}
}
