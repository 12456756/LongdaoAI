package service

import (
	"context"
	"database/sql"
	"errors"

	jose "github.com/go-jose/go-jose/v4"
)

type SEPRotationRequest struct {
	ContractVersion      int    `json:"contract_version"`
	OperationID          string `json:"operation_id"`
	Kind                 string `json:"kind"`
	ExpectedCredentialID string `json:"expected_credential_id"`
	ExpectedVersion      int    `json:"expected_version"`
	EncryptionKeyID      string `json:"encryption_key_id"`
}

type SEPRotationResult struct {
	ContractVersion  int           `json:"contract_version"`
	OperationID      string        `json:"operation_id"`
	EnterpriseID     string        `json:"enterprise_id"`
	ComputeAccountID int64         `json:"compute_account_id"`
	Kind             string        `json:"kind"`
	OldCredentialID  string        `json:"old_credential_id"`
	ExpectedVersion  int           `json:"expected_version"`
	Status           string        `json:"status"`
	Credential       SEPCredential `json:"credential"`
}

func (s *SEPService) Rotation(ctx context.Context, integration, enterprise, operation string) (*SEPRotationResult, error) {
	v := &SEPRotationResult{ContractVersion: 1}
	err := s.db.QueryRowContext(ctx, `SELECT r.operation_id,e.enterprise_id,e.user_id,r.kind,r.old_credential_id,old.version,r.status,
        c.id,c.version,k.group_id,c.encryption_key_id,c.masked_key,c.encrypted_key
        FROM sep_credential_rotations r JOIN sep_enterprises e ON e.id=r.enterprise_pk
        JOIN sep_credentials c ON c.id=r.new_credential_id JOIN sep_credentials old ON old.id=r.old_credential_id
        JOIN api_keys k ON k.id=c.api_key_id
        WHERE r.integration_id=$1 AND e.enterprise_id=$2 AND r.operation_id=$3`, integration, enterprise, operation).
		Scan(&v.OperationID, &v.EnterpriseID, &v.ComputeAccountID, &v.Kind, &v.OldCredentialID, &v.ExpectedVersion, &v.Status,
			&v.Credential.CredentialID, &v.Credential.Version, &v.Credential.GroupID, &v.Credential.EncryptionKeyID, &v.Credential.MaskedKey, &v.Credential.EncryptedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	v.Credential.Type = v.Kind
	v.Credential.DeliveryStatus = "available"
	return v, nil
}

func (s *SEPService) RotateCredential(ctx context.Context, cfg *SEPConfig, enterprise string, r SEPRotationRequest) (*SEPRotationResult, error) {
	if r.ContractVersion != 1 || r.ExpectedVersion < 1 || r.ExpectedVersion >= 2147483647 || (r.Kind != "chat" && r.Kind != "image" && r.Kind != "video") {
		return nil, SEPFailure(400, "INVALID_CREDENTIAL_ROTATION", "更换用途或版本无效", "rotate")
	}
	for _, id := range []string{enterprise, r.OperationID, r.ExpectedCredentialID, r.EncryptionKeyID} {
		if err := sepID(id); err != nil {
			return nil, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = sepLock(ctx, tx, "sep-rotation:"+cfg.IntegrationID+":"+r.OperationID); err != nil {
		return nil, err
	}
	hash := sepHash(sepJSON(struct {
		Enterprise string
		Request    SEPRotationRequest
	}{enterprise, r}))
	var saved string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM sep_credential_rotations WHERE integration_id=$1 AND operation_id=$2`, cfg.IntegrationID, r.OperationID).Scan(&saved)
	if err == nil {
		if saved != hash {
			return nil, sepConflict("IDEMPOTENCY_CONFLICT", "更换编号已用于不同请求")
		}
		tx.Rollback()
		return s.Rotation(ctx, cfg.IntegrationID, enterprise, r.OperationID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var pk, user int64
	var status string
	err = tx.QueryRowContext(ctx, `SELECT id,user_id,status FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2 FOR UPDATE`, cfg.IntegrationID, enterprise).Scan(&pk, &user, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	if status != "ready" {
		return nil, sepConflict("ENTERPRISE_DISABLED", "企业已停用，不能更换凭证")
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_credential_rotations WHERE enterprise_pk=$1 AND kind=$2 AND status<>'completed')`, pk, r.Kind).Scan(&busy); err != nil {
		return nil, err
	}
	if busy {
		return nil, sepConflict("CREDENTIAL_ROTATION_IN_PROGRESS", "该用途已有未完成的更换，请查询原操作")
	}
	var old string
	var version int
	var gid int64
	err = tx.QueryRowContext(ctx, `SELECT c.id,c.version,k.group_id FROM sep_credentials c JOIN api_keys k ON k.id=c.api_key_id WHERE c.enterprise_pk=$1 AND c.kind=$2 AND c.is_current FOR UPDATE OF c`, pk, r.Kind).Scan(&old, &version, &gid)
	if err != nil {
		return nil, err
	}
	if old != r.ExpectedCredentialID || version != r.ExpectedVersion {
		return nil, sepConflict("CREDENTIAL_VERSION_CONFLICT", "当前凭证版本已变化，请刷新后重试")
	}
	if r.EncryptionKeyID != cfg.EncryptionKeyID {
		return nil, sepConflict("ENCRYPTION_KEY_VERSION_MISMATCH", "凭证加密版本不匹配")
	}
	public, err := parseSEPPublicKey(cfg.EncryptionPublicKey)
	if err != nil {
		return nil, err
	}
	enc, err := jose.NewEncrypter(jose.A256GCM, jose.Recipient{Algorithm: jose.RSA_OAEP_256, Key: public, KeyID: r.EncryptionKeyID}, nil)
	if err != nil {
		return nil, err
	}
	key, cid := sepRandom("sk-sep-"), sepRandom("cred_")
	var kid int64
	if _, err = tx.ExecContext(ctx, `SET LOCAL sep.key_management='on'`); err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,key,name,group_id,status,ip_whitelist,ip_blacklist,quota,quota_used,expires_at,
        rate_limit_5h,rate_limit_1d,rate_limit_7d,usage_5h,usage_1d,usage_7d,window_5h_start,window_1d_start,window_7d_start)
        SELECT user_id,$1,$2,group_id,status,ip_whitelist,ip_blacklist,quota,quota_used,expires_at,
        rate_limit_5h,rate_limit_1d,rate_limit_7d,usage_5h,usage_1d,usage_7d,window_5h_start,window_1d_start,window_7d_start
        FROM api_keys WHERE id=(SELECT api_key_id FROM sep_credentials WHERE id=$3) AND deleted_at IS NULL RETURNING id`, key, "SEP "+r.Kind, old).Scan(&kid)
	if err != nil {
		return nil, err
	}
	sealed, err := enc.Encrypt(sepJSON(map[string]any{"enterprise_id": enterprise, "compute_account_id": user, "type": r.Kind, "credential_id": cid, "credential_version": version + 1, "key": key}))
	if err != nil {
		return nil, err
	}
	compact, err := sealed.CompactSerialize()
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sep_credentials(id,enterprise_pk,kind,api_key_id,version,encryption_key_id,masked_key,encrypted_key,is_current) VALUES($1,$2,$3,$4,$5,$6,$7,$8,FALSE)`, cid, pk, r.Kind, kid, version+1, r.EncryptionKeyID, key[:10]+"..."+key[len(key)-4:], compact)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sep_credential_rotations(integration_id,operation_id,enterprise_pk,kind,old_credential_id,new_credential_id,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7)`, cfg.IntegrationID, r.OperationID, pk, r.Kind, old, cid, hash)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Rotation(ctx, cfg.IntegrationID, enterprise, r.OperationID)
}

func (s *SEPService) AdvanceRotation(ctx context.Context, integration, enterprise, operation string, retire bool) (*SEPRotationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var pk int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2 FOR UPDATE`, integration, enterprise).Scan(&pk)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	var old, next, status string
	err = tx.QueryRowContext(ctx, `SELECT old_credential_id,new_credential_id,status FROM sep_credential_rotations WHERE integration_id=$1 AND operation_id=$2 AND enterprise_pk=$3 FOR UPDATE`, integration, operation, pk).Scan(&old, &next, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	if retire && status == "prepared" {
		return nil, sepConflict("CREDENTIAL_NOT_ACKNOWLEDGED", "SEP 尚未确认保存新凭证")
	}
	if retire && status != "completed" {
		var busy bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_executions x JOIN sep_credentials c ON c.api_key_id=x.api_key_id WHERE c.id=$1 AND (x.execution_status NOT IN ('completed','failed','cancelled') OR x.fee_status<>'final' OR NOT EXISTS(SELECT 1 FROM sep_settlements st WHERE st.execution_pk=x.id)))`, old).Scan(&busy)
		if err != nil {
			return nil, err
		}
		if busy {
			return nil, sepConflict("CREDENTIAL_TASKS_PENDING", "旧凭证仍有在途或待结算任务，继续保留")
		}
		_, err = tx.ExecContext(ctx, `UPDATE sep_credentials SET generation_state='read_only' WHERE id=$1`, old)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sep_credential_rotations SET status='completed',updated_at=NOW() WHERE integration_id=$1 AND operation_id=$2`, integration, operation)
	} else if !retire && status == "prepared" {
		_, err = tx.ExecContext(ctx, `UPDATE sep_credentials SET is_current=FALSE,generation_state='draining' WHERE id=$1 AND is_current`, old)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sep_credentials SET is_current=TRUE WHERE id=$1`, next)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE sep_credential_rotations SET status='acknowledged',updated_at=NOW() WHERE integration_id=$1 AND operation_id=$2`, integration, operation)
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Rotation(ctx, integration, enterprise, operation)
}
