package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

const (
	videoSubmissionMaxAttempts    = 4
	videoSubmissionAttemptTimeout = 60 * time.Second
	videoSubmissionTimeout        = 4*videoSubmissionAttemptTimeout + 7*time.Second
)

type VideoSubmissionRetry struct {
	Attempts    int              `json:"attempts"`
	MaxAttempts int              `json:"max_attempts"`
	Exhausted   bool             `json:"exhausted"`
	Errors      []map[string]any `json:"errors,omitempty"`
}

func waitVideoSubmissionRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retryableVideoSubmission(task *legacyVideoTask, err error) bool {
	if task != nil && legacyTaskPattern.MatchString(task.ID) {
		return false
	}
	var failure *videoProtocolError
	if !errors.As(err, &failure) || failure.notDispatched {
		return false
	}
	if failure.rejected {
		status, _ := failure.diagnostic["upstream_status"].(int)
		return status >= 500 && status <= 599
	}
	return failure.code == "video_upstream_error" || failure.code == "video_upstream_timeout" || failure.code == "video_protocol_error"
}

func (s *OpenAIGatewayService) createLegacyVideoWithRetry(ctx context.Context, account *Account, body []byte, idem string, sub *videoSubmission, save func() error) (*legacyVideoTask, error) {
	// The client may disconnect while the provider is accepting the task. Finish
	// the bounded idempotent attempt sequence and persist its outcome anyway.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), videoSubmissionTimeout)
	defer cancel()
	ctx = withSEPVideoRetry(ctx)
	wait := s.videoSubmissionRetryWait
	if wait == nil {
		wait = waitVideoSubmissionRetry
	}
	var task *legacyVideoTask
	var err error
	for attempt := 0; attempt < videoSubmissionMaxAttempts; attempt++ {
		if attempt > 0 {
			if waitErr := wait(ctx, time.Second*time.Duration(1<<(attempt-1))); waitErr != nil {
				return task, videoCause(504, "video_upstream_timeout", "Wait for video submission retry", waitErr)
			}
		}
		sub.Retry.Attempts = attempt + 1
		if saveErr := save(); saveErr != nil {
			sub.Retry.Attempts = attempt
			return task, videoCause(503, "video_submission_unavailable", "Save video submission attempt", saveErr)
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, videoSubmissionAttemptTimeout)
		task, err = s.legacyRequest(attemptCtx, account, "", body, idem)
		attemptCancel()
		if err == nil {
			return task, nil
		}
		var failure *videoProtocolError
		if errors.As(err, &failure) {
			fields := videoFailureFields(failure)
			fields["attempt"] = attempt + 1
			sub.Retry.Errors = append(sub.Retry.Errors, fields)
		}
		retryable := retryableVideoSubmission(task, err)
		sub.Retry.Exhausted = retryable && attempt+1 == videoSubmissionMaxAttempts
		slog.Warn("video_legacy.submission_attempt_failed", "request_id", sub.RequestID, "account_id", account.ID, "attempt", attempt+1, "retryable", retryable, "reason", safeVideoError(err))
		if !retryable {
			return task, err
		}
	}
	return task, err
}

func videoSubmissionClosed(sub *videoSubmission) bool {
	return sub.State == "failed" || sub.State == "rejected" || sub.State == "not_submitted" || sub.State == "protocol_error"
}

func (s *OpenAIGatewayService) closedVideoSubmissionBilling(ctx context.Context, key *APIKey, sub *videoSubmission) VideoBillingStatus {
	if sub.BillingRegistered {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		repo, ok := s.usageBillingRepo.(VideoBillingOutcomeRepository)
		if !ok {
			return billingProblem(errors.New("Video billing outcome repository is unavailable"))
		}
		if sub.Billing != nil && sub.Billing.SEPComputeRequestID != "" {
			restored, err := restoreSEPMediaBillingKey(ctx, key, sub.Billing.SEPComputeRequestID, sub.BillingRequestID)
			if err != nil {
				return billingProblem(err)
			}
			if err = restored.SEP.Service.FinalizeUnchargedMedia(ctx, restored, "failed", true, sub.SemanticSHA256); err != nil {
				return billingProblem(err)
			}
			return s.VideoRequestBillingStatus(ctx, restored, sub.RequestID)
		}
		row, err := repo.CloseVideoBillingWithoutCharge(ctx, key.ID, key.UserID, sub.BillingRequestID)
		if err != nil {
			return billingProblem(err)
		}
		return VideoBillingStatus{Status: row.Status, ActualCostUSD: row.ActualCostUSD}
	}
	zero := "0"
	return VideoBillingStatus{Status: "not_charged", ActualCostUSD: &zero}
}

func (s *OpenAIGatewayService) closeInterruptedVideoSubmission(ctx context.Context, sub *videoSubmission, storageKey string, previous []byte) error {
	if sub.Protocol != VideoProtocolLegacy || sub.Task != nil {
		return nil
	}
	deadline := sub.DispatchDeadline
	if deadline == 0 {
		// Older records have no dispatch deadline but a fixed seven-day expiry.
		deadline = time.Unix(sub.ExpiresAt, 0).Add(-7*24*time.Hour + videoSubmissionTimeout + 10*time.Second).Unix()
	}
	interrupted := sub.State == "dispatching" && time.Now().Unix() > deadline
	if !interrupted && sub.State != "unknown" {
		return nil
	}
	sub.State = "failed"
	if sub.Failure == nil {
		sub.Failure = videoError(504, "video_submission_interrupted", "Video submission did not finish before its deadline; closed without charging or resubmitting").(*videoProtocolError)
	}
	store, err := s.videoStore()
	if err != nil {
		return err
	}
	data := mustVideoDiagnosticJSON(sub)
	updated, err := store.VideoReplaceIfEqual(ctx, storageKey, previous, data)
	if err != nil {
		return err
	}
	if !updated {
		current, loadErr := store.VideoLoad(ctx, storageKey)
		if loadErr != nil {
			return loadErr
		}
		return json.Unmarshal(current, sub)
	}
	return nil
}
