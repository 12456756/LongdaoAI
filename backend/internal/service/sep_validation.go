package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/shopspring/decimal"
)

// Validation uses an internal platform credential, never an enterprise key.
func (s *SEPService) ValidationKey(ctx context.Context, cfg *SEPConfig, kind string) (*APIKey, error) {
	if kind != "image" && kind != "video" {
		return nil, SEPFailure(400, "INVALID_KEY_TYPE", "校验用途无效", "validate")
	}
	groupID := cfg.ImageGroupID
	if kind == "video" {
		groupID = cfg.VideoGroupID
	}
	group, err := s.groups.GetByID(ctx, groupID)
	if err != nil || group == nil || !group.IsActive() || group.IsSubscriptionType() || !GroupAllowsImageGeneration(group) {
		return nil, SEPFailure(400, "INVALID_SEP_GROUP", "平台媒体校验分组不可用", "validate")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = sepLock(ctx, tx, "sep-validation:"+cfg.IntegrationID); err != nil {
		return nil, err
	}
	var userID, keyID int64
	err = tx.QueryRowContext(ctx, `SELECT user_id,api_key_id FROM sep_validation_keys WHERE integration_id=$1 AND kind=$2`, cfg.IntegrationID, kind).Scan(&userID, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		if err = tx.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role,balance,concurrency,status,username) VALUES($1,'!sep-validation-account','user',0,1,'active','SEP platform validation') RETURNING id`, sepRandom("sep-validation-")+"@integration.invalid").Scan(&userID); err != nil {
			return nil, err
		}
		if err = tx.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,key,name,group_id,status) VALUES($1,$2,$3,$4,'active') RETURNING id`, userID, sepRandom("sk-sep-validation-"), "SEP platform "+kind+" validation", groupID).Scan(&keyID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO sep_validation_keys(integration_id,kind,user_id,api_key_id) VALUES($1,$2,$3,$4)`, cfg.IntegrationID, kind, userID, keyID); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET group_id=$1 WHERE id=$2`, groupID, keyID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	key, err := s.keys.apiKeyRepo.GetByID(ctx, keyID)
	if err != nil {
		return nil, err
	}
	key.Group = group
	key.GroupID = &groupID
	return key, nil
}
func (s *SEPService) ValidationResponse(ctx context.Context, response any) (map[string]any, error) {
	var v map[string]any
	if err := json.Unmarshal(sepJSON(response), &v); err != nil {
		return nil, err
	}
	rate, err := s.settings.GetPlatformExchangeRate(ctx)
	if err != nil {
		return nil, err
	}
	v["contract_version"] = 1
	v["currency"] = "CNY"
	v["quote_scope"] = "platform_validation"
	v["amount_cny"] = nil
	if amount, ok := v["amount_usd"].(string); ok {
		a, e := decimal.NewFromString(amount)
		if e != nil {
			return nil, e
		}
		r, _ := decimal.NewFromString(rate)
		v["amount_cny"] = a.Mul(r).Round(6).StringFixed(6)
	}
	delete(v, "amount_usd")
	v["pricing_version"] = ""
	return v, nil
}
