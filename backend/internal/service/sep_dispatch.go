package service

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"sync"
)

type sepVideoRetryKey struct{}

// Only the existing bounded legacy-video retry loop may reuse a dispatch claim.
// A new inbound request never inherits this permit.
type sepVideoRetryPermit struct {
	mu        sync.Mutex
	claimed   bool
	accountID int64
	execution string
	key       string
}

func withSEPVideoRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, sepVideoRetryKey{}, &sepVideoRetryPermit{})
}

// Claim at the shared HTTP boundary prevents uncoordinated duplicate generation.
func ClaimSEPUpstreamDispatch(req *http.Request, accountID int64) error {
	if req == nil || req.Method != http.MethodPost {
		return nil
	}
	e := SEPExecutionFromContext(req.Context())
	if e == nil || e.ComputeRequestID == "" || e.Service == nil {
		return nil
	}
	path := strings.TrimRight(req.URL.Path, "/")
	generation := false
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages", "/images/generations", "/images/edits", "/images/generations/async", "/images/edits/async", "/videos", "/video/generations", "/video/tasks", "/generateContent", "/streamGenerateContent"} {
		if strings.HasSuffix(path, suffix) {
			generation = true
			break
		}
	}
	if strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent") {
		generation = true
	}
	if !generation {
		return nil
	}
	req.GetBody = nil
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-sep-") {
			req.Header.Del(name)
		}
	}
	if e.ReplayOnly {
		return sepConflict("EXECUTION_ALREADY_RECEIVED", "Original media receipt is unavailable; this execution will not be submitted again")
	}
	permit, _ := req.Context().Value(sepVideoRetryKey{}).(*sepVideoRetryPermit)
	if permit != nil && strings.HasSuffix(path, "/video/tasks") && req.Header.Get("Idempotency-Key") != "" {
		permit.mu.Lock()
		defer permit.mu.Unlock()
		if permit.claimed {
			if permit.accountID == accountID && permit.execution == e.ComputeRequestID && permit.key == req.Header.Get("Idempotency-Key") {
				return nil
			}
			return sepConflict("IDEMPOTENCY_CONFLICT", "Video retry must reuse the original account, execution and idempotency key")
		}
	} else {
		permit = nil
	}
	var id string
	err := e.Service.db.QueryRowContext(req.Context(), `UPDATE sep_executions SET dispatched_at=NOW(),execution_status='running',account_id=$1,resource_version=resource_version+1,updated_at=NOW() WHERE id=$2 AND dispatched_at IS NULL AND fee_status='pending' RETURNING id`, accountID, e.ComputeRequestID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sepConflict("EXECUTION_DISPATCH_ALREADY_CLAIMED", "该执行已派发或派发结果待确认，禁止再次请求上游")
	}
	if err == nil && permit != nil {
		permit.claimed, permit.accountID, permit.execution, permit.key = true, accountID, e.ComputeRequestID, req.Header.Get("Idempotency-Key")
	}
	return err
}
