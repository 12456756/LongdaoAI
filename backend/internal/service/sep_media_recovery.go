package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// This error is created only after enterprise authentication and execution
// ownership have been checked. It permits saving a rejection, not model access.
type SEPRejectedMediaError struct {
	Key          *APIKey
	Cause        error
	BillingError error
}

func (e *SEPRejectedMediaError) Error() string { return e.Cause.Error() }
func (e *SEPRejectedMediaError) Unwrap() error { return e.Cause }

func sepMediaIdentity(r *http.Request) (string, string) {
	id := r.Header.Get("X-Client-Request-ID")
	if id == "" {
		id, _ = r.Context().Value(ctxkey.ClientRequestID).(string)
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idem == "" {
		idem = id
	}
	return id, idem
}

// Authentication still verifies the enterprise and key. Existing media requests
// then use their original snapshot and the baseline semantic replay checks.
func (s *SEPService) replayMediaExecution(ctx context.Context, key *APIKey, r *http.Request, id, hash string) (*APIKey, error) {
	e := *key.SEP
	var owner, groupID int64
	var oldHash, clientID, idem, path, origin string
	var price, multipliers []byte
	err := s.db.QueryRowContext(ctx, `SELECT id,api_key_id,enterprise_pk,kind,model,request_hash,exchange_rate::text,price_snapshot,multiplier_snapshot,COALESCE(group_id,0),billing_origin,COALESCE(client_request_id,''),COALESCE(idempotency_key,''),COALESCE(request_path,'') FROM sep_executions WHERE integration_id=$1 AND execution_id=$2`, e.IntegrationID, id).Scan(&e.ComputeRequestID, &owner, &e.EnterprisePK, &e.Kind, &e.Model, &oldHash, &e.Rate, &price, &multipliers, &groupID, &origin, &clientID, &idem, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, requestIdem := sepMediaIdentity(r)
	if owner != key.ID || e.EnterprisePK != key.SEP.EnterprisePK || e.Kind != key.SEP.Kind ||
		(path != "" && (path != r.URL.Path || idem != requestIdem)) ||
		(path == "" && oldHash != hash) {
		return nil, sepConflict("IDEMPOTENCY_CONFLICT", "Execution ID belongs to a different key or media request")
	}
	// The baseline protocol permits the same idempotency key with a new transport
	// request ID. Always replay under the original logical request's namespace.
	if clientID != "" {
		r.Header.Set("X-Client-Request-ID", clientID)
		*r = *r.WithContext(context.WithValue(r.Context(), ctxkey.ClientRequestID, clientID))
	}
	var snapshot ModelBasePrice
	if err = json.Unmarshal(price, &snapshot); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(multipliers, &e.Multipliers); err != nil {
		return nil, err
	}
	e.ExecutionID, e.Price, e.ReplayOnly, e.DirectBilling = id, &snapshot, true, origin == "compute"
	copyKey := *key
	copyKey.SEP = &e
	if groupID > 0 {
		if copyKey.Group == nil || copyKey.Group.ID != groupID {
			copyKey.Group, err = s.groups.GetByID(ctx, groupID)
			if err != nil {
				return nil, err
			}
		}
		copyKey.GroupID = &groupID
	}
	return &copyKey, nil
}

// Use the baseline receipt's explicit no-charge decision, never just its HTTP
// status. Accepted tasks and replay requests cannot overwrite their billing.
func (s *SEPService) FinalizeRejectedMediaReceipt(ctx context.Context, key *APIKey, receipt map[string]any) error {
	if key == nil || key.SEP == nil || key.SEP.ComputeRequestID == "" || key.SEP.ReplayOnly || receipt["operation"] != "create" || receipt["task_id"] != nil {
		return nil
	}
	billing, ok := receipt["billing"].(map[string]any)
	if !ok || billing["status"] != "not_charged" {
		return nil
	}
	switch receipt["submission_status"] {
	case "not_submitted":
		return s.finalizeUnsubmittedMedia(ctx, key)
	case "failed", "rejected":
		return s.finalizeRejectedMedia(ctx, key, "no_task")
	default:
		return nil
	}
}

func (s *SEPService) finalizeUnsubmittedMedia(ctx context.Context, key *APIKey) error {
	return s.finalizeRejectedMedia(ctx, key, "not_dispatched")
}

func (s *SEPService) finalizeRejectedMedia(ctx context.Context, key *APIKey, scope string) error {
	e := *key.SEP
	e.MediaFinal, e.MediaRejectionScope, e.MediaTerminalStatus = true, scope, "failed"
	_, err := s.FinalizeUsage(ctx, &e, &UsageBillingCommand{RequestID: e.ComputeRequestID, APIKeyID: key.ID, UserID: key.UserID}, nil)
	return err
}

// Restore only the namespace, not a provider task. Redis remains authoritative
// for the original submission receipt, including failures without a task ID.
func (s *SEPService) RestoreVideoRequestGroup(ctx context.Context, key *APIKey, requestID string) (*APIKey, error) {
	var groupID int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(group_id,0) FROM sep_executions WHERE enterprise_pk=$1 AND api_key_id=$2 AND kind='video' AND client_request_id=$3 ORDER BY created_at DESC LIMIT 1`, key.SEP.EnterprisePK, key.ID, requestID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	copyKey := *key
	if groupID > 0 {
		if key.Group == nil || key.Group.ID != groupID {
			copyKey.Group, err = s.groups.GetByID(ctx, groupID)
			if err != nil {
				return nil, err
			}
		}
		copyKey.GroupID = &groupID
	}
	return &copyKey, nil
}
