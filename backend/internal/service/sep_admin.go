package service

import (
	"context"
	"database/sql"
	"time"
)

func (s *SEPService) EnterpriseBills(ctx context.Context, integration, enterprise string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT x.execution_id,x.kind,x.model,x.execution_status,x.fee_status,x.gross_cny::text,x.created_at,s.settlement_id,x.billing_origin,x.billing_error FROM sep_executions x JOIN sep_enterprises e ON e.id=x.enterprise_pk LEFT JOIN sep_settlements s ON s.execution_pk=x.id WHERE x.integration_id=$1 AND e.enterprise_id=$2 ORDER BY x.created_at DESC LIMIT 100`, integration, enterprise)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, kind, model, state, fee string
		var origin, detail string
		var gross, settlement sql.NullString
		var at time.Time
		if err = rows.Scan(&id, &kind, &model, &state, &fee, &gross, &at, &settlement, &origin, &detail); err != nil {
			return nil, err
		}
		var amount any
		if gross.Valid {
			amount = gross.String
		}
		items = append(items, map[string]any{"execution_id": id, "kind": kind, "model_id": model, "execution_status": state, "fee_status": fee, "gross_cny": amount, "settlement_id": settlement.String, "created_at": at, "billing_origin": origin, "billing_error": detail})
	}
	return items, rows.Err()
}
