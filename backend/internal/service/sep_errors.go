package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
	"github.com/lib/pq"
)

// NormalizeSEPError preserves public business errors while classifying failures
// that previously all looked like storage outages. Database row data stays private.
func NormalizeSEPError(err error, stage string) (*SEPError, string) {
	diagnostic := errordetail.Text(err.Error())
	var pg *pq.Error
	if errors.As(err, &pg) {
		diagnostic = fmt.Sprintf("数据库操作失败：%s (SQLSTATE %s)", errordetail.Text(pg.Message), pg.Code)
	}
	if chars := []rune(diagnostic); len(chars) > 1024 {
		diagnostic = string(chars[:1024])
	}
	var se *SEPError
	if errors.As(err, &se) {
		copy := *se
		copy.Message = errordetail.Text(copy.Message)
		return &copy, ""
	}
	var app *infraerrors.ApplicationError
	if errors.As(err, &app) {
		code := app.Reason
		if code == "" {
			code = "SEP_OPERATION_FAILED"
		}
		return &SEPError{Status: int(app.Code), Code: code, Message: errordetail.Text(app.Message), Stage: stage, Retryable: app.Code >= 500, NextAction: "correct_request_or_query_original_operation"}, diagnostic
	}
	result := &SEPError{Status: 503, Code: "SEP_INTERNAL_ERROR", Message: "SEP 操作失败：" + diagnostic, Stage: stage, Retryable: true, NextAction: "query_original_operation"}
	var network net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		result.Code, result.Message = "SEP_OPERATION_TIMEOUT", "SEP 操作超时，请查询原业务编号确认结果"
	case errors.Is(err, context.Canceled):
		result.Code, result.Message = "SEP_OPERATION_CANCELLED", "SEP 请求已中断，请查询原业务编号确认结果"
	case errors.As(err, &pg):
		result.Code, result.Message = "SEP_DATABASE_"+string(pg.Code), diagnostic
		if pg.Code == "23505" {
			result.Status, result.Code, result.Message = 409, "SEP_RESOURCE_CONFLICT", "资源发生并发冲突："+diagnostic+"；请查询原业务编号"
		}
		if pg.Code == "P0001" {
			result.Status, result.Code, result.Message = 409, "SEP_ACCOUNT_OPERATION_REJECTED", diagnostic
		}
	case errors.Is(err, sql.ErrConnDone), errors.Is(err, sql.ErrTxDone):
		result.Code, result.Message = "SEP_STORAGE_UNAVAILABLE", "SEP 数据库连接或事务已结束，请查询原业务编号"
	case errors.As(err, &network):
		result.Code, result.Message = "SEP_DEPENDENCY_UNAVAILABLE", "SEP 依赖服务连接失败："+diagnostic
	}
	return result, strings.TrimSpace(diagnostic)
}

// Admin endpoints use the normal application envelope but retain the SEP failure details.
func SEPAdminBalanceError(err error) error {
	se, diagnostic := NormalizeSEPError(err, "balance")
	state := "not_committed"
	if se.Code == "BALANCE_COMMIT_UNCONFIRMED" {
		state = "unknown"
	}
	if se.AccountingStatus != "" {
		state = se.AccountingStatus
	}
	return infraerrors.New(se.Status, se.Code, se.Message).WithMetadata(map[string]string{"stage": se.Stage, "diagnostic": diagnostic, "accounting_status": state, "next_action": se.NextAction, "business_ref": se.BusinessRef, "request_id": sepRandom("trace_")})
}
