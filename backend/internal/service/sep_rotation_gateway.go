package service

import (
	"context"
	"database/sql"
	"strings"
)

type sepCredentialReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkSEPRotatedCredential(ctx context.Context, db sepCredentialReader, key *APIKey, path string, execute, quote bool, requestLookup ...bool) error {
	var state string
	var current bool
	err := db.QueryRowContext(ctx, `SELECT generation_state,is_current FROM sep_credentials WHERE api_key_id=$1 AND enterprise_pk=$2`, key.ID, key.SEP.EnterprisePK).Scan(&state, &current)
	if err != nil {
		return err
	}
	if state == "read_only" {
		if execute || quote {
			return sepConflict("CREDENTIAL_RETIRED", "旧凭证已停止新生成，请使用新版本")
		}
		var task string
		for _, prefix := range []string{"/v1/videos/", "/v1/images/tasks/"} {
			if strings.HasPrefix(path, prefix) {
				task = strings.Split(strings.TrimPrefix(path, prefix), "/")[0]
			}
		}
		if task == "" {
			return sepConflict("CREDENTIAL_RETIRED", "旧凭证仅可查询原任务")
		}
		// A failed submission can have no task association in SQL. Its Redis
		// receipt is still scoped to this exact key and verifies ownership.
		if len(requestLookup) > 0 && requestLookup[0] {
			return nil
		}
		var owns bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_executions WHERE enterprise_pk=$1 AND api_key_id=$2 AND task_id=$3)`, key.SEP.EnterprisePK, key.ID, task).Scan(&owns); err != nil {
			return err
		}
		if !owns {
			return sepMissing()
		}
	}
	if !current && (quote || execute && key.SEP.DirectBilling) {
		return sepConflict("CREDENTIAL_NOT_CURRENT", "请使用当前版本凭证发起新请求")
	}
	return nil
}
