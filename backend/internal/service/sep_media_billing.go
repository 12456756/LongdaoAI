package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func sepGatewayMediaOperation(method, path string) bool {
	if method == http.MethodPost && path == "/v1/videos/references" {
		return true
	}
	if method == http.MethodPut && strings.HasPrefix(path, "/v1/videos/") && strings.HasSuffix(path, "/cancel") {
		task := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/videos/"), "/cancel")
		return task != "" && !strings.Contains(task, "/")
	}
	return false
}

// Polling authenticates the current request, while settlement uses the immutable
// execution and group captured when the original generation was accepted.
func restoreSEPMediaBillingKey(ctx context.Context, key *APIKey, computeID, requestID string) (*APIKey, error) {
	if key == nil || key.SEP == nil || key.SEP.Service == nil {
		return nil, errors.New("SEP media settlement requires the original enterprise credential")
	}
	s := key.SEP.Service
	e := *key.SEP
	var price, multipliers []byte
	var groupID int64
	var origin string
	err := s.db.QueryRowContext(ctx, `SELECT id,execution_id,model,exchange_rate::text,price_snapshot,multiplier_snapshot,COALESCE(group_id,0),billing_origin FROM sep_executions WHERE id=$1 AND enterprise_pk=$2 AND api_key_id=$3 AND kind=$4`, computeID, e.EnterprisePK, key.ID, e.Kind).Scan(&e.ComputeRequestID, &e.ExecutionID, &e.Model, &e.Rate, &price, &multipliers, &groupID, &origin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	var snapshot ModelBasePrice
	if err = json.Unmarshal(price, &snapshot); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(multipliers, &e.Multipliers); err != nil {
		return nil, err
	}
	e.Price, e.MediaFinal, e.DirectBilling = &snapshot, true, origin == "compute"
	e.MediaBillingRequestID = requestID
	copyKey := *key
	copyKey.SEP = &e
	if groupID > 0 && (copyKey.Group == nil || copyKey.Group.ID != groupID) {
		group, err := s.groups.GetByID(ctx, groupID)
		if err != nil {
			return nil, err
		}
		copyKey.Group = group
	}
	if groupID > 0 {
		copyKey.GroupID = &groupID
	}
	return &copyKey, nil
}

func (s *SEPService) FinalizeUnchargedMedia(ctx context.Context, key *APIKey, status string, video bool, fingerprint string) error {
	if key == nil || key.SEP == nil || !key.SEP.MediaFinal || key.SEP.MediaBillingRequestID == "" || (status != "failed" && status != "cancelled") {
		return errors.New("SEP media finalization is missing the original execution or billing request")
	}
	e := *key.SEP
	e.MediaTerminalStatus = status
	_, err := s.FinalizeUsage(ctx, &e, &UsageBillingCommand{RequestID: e.MediaBillingRequestID, APIKeyID: key.ID, UserID: key.UserID, RequestFingerprint: fingerprint, DeferredVideo: video, DeferredImage: !video}, nil)
	return err
}

func lockSEPMediaOutcome(ctx context.Context, tx *sql.Tx, e *SEPExecution, cmd *UsageBillingCommand) error {
	if !cmd.DeferredVideo && !cmd.DeferredImage {
		return nil
	}
	table := "image_request_billing"
	if cmd.DeferredVideo {
		table = "video_request_billing"
	}
	var status string
	err := tx.QueryRowContext(ctx, `SELECT status FROM `+table+` WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3 FOR UPDATE`, cmd.APIKeyID, cmd.UserID, cmd.RequestID).Scan(&status)
	if err != nil {
		return err
	}
	if status != "pending" {
		return sepConflict("MEDIA_BILLING_STATE_CONFLICT", "媒体任务已结束计费，不能再次更改费用")
	}
	status = "settled"
	if e.MediaTerminalStatus == "failed" || e.MediaTerminalStatus == "cancelled" {
		status = "not_charged"
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+table+` SET status=$4,actual_cost=$5,updated_at=NOW() WHERE api_key_id=$1 AND user_id=$2 AND request_id=$3`, cmd.APIKeyID, cmd.UserID, cmd.RequestID, status, cmd.BalanceCost+cmd.SubscriptionCost)
	return err
}
