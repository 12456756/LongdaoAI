package service

import (
	"context"
	"database/sql"
	"errors"
	"regexp"

	"golang.org/x/crypto/bcrypt"
)

type SEPPasswordUpdate struct {
	ContractVersion  int    `json:"contract_version"`
	EnterpriseID     string `json:"enterprise_id"`
	ComputeAccountID int64  `json:"compute_account_id"`
	OperationID      string `json:"operation_id"`
	PasswordVersion  int64  `json:"password_version"`
	PasswordHash     string `json:"password_hash"`
}

type SEPPasswordReceipt struct {
	ContractVersion  int    `json:"contract_version"`
	EnterpriseID     string `json:"enterprise_id"`
	ComputeAccountID int64  `json:"compute_account_id"`
	OperationID      string `json:"operation_id"`
	PasswordVersion  int64  `json:"password_version"`
	AppliedVersion   int64  `json:"applied_version"`
	Status           string `json:"status"`
}

var sepPasswordHash = regexp.MustCompile(`^\$2[aby]\$(0[4-9]|[12][0-9]|3[01])\$[./A-Za-z0-9]{53}$`)

func (s *SEPService) UpdatePassword(ctx context.Context, integration, enterpriseID string, r SEPPasswordUpdate) (*SEPPasswordReceipt, error) {
	if sepID(enterpriseID) != nil || sepID(r.OperationID) != nil || r.EnterpriseID != enterpriseID || r.ContractVersion != 1 || r.ComputeAccountID < 1 || r.PasswordVersion < 1 || r.PasswordVersion > 2147483647 {
		return nil, SEPFailure(400, "INVALID_PASSWORD_UPDATE", "密码同步的企业、账号、版本或业务编号无效", "password")
	}
	if _, err := bcrypt.Cost([]byte(r.PasswordHash)); err != nil || !sepPasswordHash.MatchString(r.PasswordHash) {
		return nil, SEPFailure(400, "INVALID_PASSWORD_HASH", "密码同步需要有效的 bcrypt 哈希", "password")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var userID, version int64
	var previousRef, previousHash string
	err = tx.QueryRowContext(ctx, `SELECT user_id,password_sync_version,password_sync_ref,password_sync_hash FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2 FOR UPDATE`, integration, enterpriseID).Scan(&userID, &version, &previousRef, &previousHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	if err != nil {
		return nil, err
	}
	if userID != r.ComputeAccountID {
		return nil, sepConflict("COMPUTE_ACCOUNT_MISMATCH", "密码同步必须使用企业原绑定的算力账号")
	}
	fingerprint := sepHash(sepJSON(r))
	receipt := &SEPPasswordReceipt{ContractVersion: 1, EnterpriseID: enterpriseID, ComputeAccountID: userID, OperationID: r.OperationID, PasswordVersion: r.PasswordVersion, AppliedVersion: version, Status: "applied"}
	if r.PasswordVersion < version {
		receipt.Status = "superseded"
		return receipt, nil
	}
	if r.PasswordVersion == version {
		if previousRef != r.OperationID || previousHash != fingerprint {
			return nil, sepConflict("IDEMPOTENCY_CONFLICT", "密码版本已被其他修改内容使用")
		}
		return receipt, nil
	}
	if previousRef == r.OperationID {
		return nil, sepConflict("IDEMPOTENCY_CONFLICT", "密码业务编号不能用于另一个版本")
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=$1,updated_at=NOW() WHERE id=$2 AND deleted_at IS NULL AND status='active'`, r.PasswordHash, userID)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, sepConflict("COMPUTE_ACCOUNT_UNAVAILABLE", "原算力登录账号当前不可用")
	}
	_, err = tx.ExecContext(ctx, `UPDATE sep_enterprises SET password_sync_version=$1,password_sync_ref=$2,password_sync_hash=$3 WHERE integration_id=$4 AND enterprise_id=$5`, r.PasswordVersion, r.OperationID, fingerprint, integration, enterpriseID)
	if err != nil {
		return nil, err
	}
	// JWT and refresh validation derive their token version from the stored password hash.
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	receipt.AppliedVersion = r.PasswordVersion
	return receipt, nil
}
