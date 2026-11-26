package service

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

// Claim at the shared HTTP boundary prevents internal failover or transport
// retries from issuing a second generation after an ambiguous first attempt.
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
	for _, suffix := range []string{"/chat/completions", "/responses", "/messages", "/images/generations", "/images/edits", "/videos", "/video/generations", "/video/tasks", "/generateContent", "/streamGenerateContent"} {
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
	req.Header.Del("Idempotency-Key")
	req.Header.Del("X-Idempotency-Key")
	for name := range req.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-sep-") {
			req.Header.Del(name)
		}
	}
	var id string
	err := e.Service.db.QueryRowContext(req.Context(), `UPDATE sep_executions SET dispatched_at=NOW(),execution_status='running',account_id=$1,resource_version=resource_version+1,updated_at=NOW() WHERE id=$2 AND dispatched_at IS NULL RETURNING id`, accountID, e.ComputeRequestID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sepConflict("EXECUTION_DISPATCH_ALREADY_CLAIMED", "该执行已派发或派发结果待确认，禁止再次请求上游")
	}
	return err
}
