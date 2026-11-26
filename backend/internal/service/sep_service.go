package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

type SEPService struct {
	upstreamUsage UsageBillingRepository
	RefreshVideo  func(context.Context, *APIKey, string) error
	db            *sql.DB
	settings      *SettingService
	groups        GroupRepository
	keys          *APIKeyService
	billing       *BillingService
	syncCancel    context.CancelFunc
	syncDone      chan struct{}
	syncOnce      sync.Once
}

func NewSEPService(db *sql.DB, settings *SettingService, groups GroupRepository, keys *APIKeyService, billing *BillingService, upstreamUsage UsageBillingRepository) *SEPService {
	s := &SEPService{db: db, settings: settings, groups: groups, keys: keys, billing: billing, upstreamUsage: upstreamUsage}
	keys.sep = s
	if repo, ok := keys.userRepo.(interface{ SetSEPAccountingService(*SEPService) }); ok {
		repo.SetSEPAccountingService(s)
	}
	return s
}
func (s *SEPService) Config(ctx context.Context) (*SEPConfig, error) {
	raw, err := s.settings.settingRepo.GetValue(ctx, SEPSettingKey)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return &SEPConfig{}, nil
		}
		return nil, err
	}
	c := &SEPConfig{}
	if raw != "" {
		err = json.Unmarshal([]byte(raw), c)
	}
	return c, err
}
func parseSEPPublicKey(raw string) (any, error) {
	b, _ := pem.Decode([]byte(raw))
	if b == nil {
		return nil, errors.New("expected PEM PUBLIC KEY")
	}
	return x509.ParsePKIXPublicKey(b.Bytes)
}
func (s *SEPService) SaveConfig(ctx context.Context, c *SEPConfig) error {
	if err := validateSEPWebhook(c.BalanceWebhookURL); err != nil {
		return err
	}
	previous, err := s.Config(ctx)
	if err != nil {
		return err
	}
	if previous.IntegrationID != "" && c.IntegrationID != previous.IntegrationID {
		return sepConflict("INTEGRATION_ID_IMMUTABLE", "集成编号创建后不能更改")
	}
	if previous.SigningKeyID != "" && previous.SigningKeyID == c.SigningKeyID && previous.SigningPublicKey != c.SigningPublicKey {
		return sepConflict("KEY_VERSION_REUSED", "更换签名公钥必须使用新的版本编号")
	}
	if previous.EncryptionKeyID != "" && previous.EncryptionKeyID == c.EncryptionKeyID && previous.EncryptionPublicKey != c.EncryptionPublicKey {
		return sepConflict("KEY_VERSION_REUSED", "更换加密公钥必须使用新的版本编号")
	}
	if c.Enabled {
		if s.billing != nil && s.billing.cfg != nil && s.billing.cfg.RunMode == config.RunModeSimple {
			return SEPFailure(400, "SEP_STANDARD_MODE_REQUIRED", "SEP 结算需要启用平台标准计费运行模式", "config")
		}
		for _, id := range []string{c.IntegrationID, c.SigningKeyID, c.EncryptionKeyID} {
			if err = sepID(id); err != nil {
				return err
			}
			if len(id) > 100 {
				return SEPFailure(400, "INVALID_BUSINESS_ID", "集成及公钥版本编号最多 100 个字符", "config")
			}
		}
		key, keyErr := parseSEPPublicKey(c.SigningPublicKey)
		if keyErr != nil {
			return SEPFailure(400, "INVALID_SIGNING_KEY", "签名公钥必须是 PEM 格式的 Ed25519 公钥", "config")
		}
		if _, ok := key.(ed25519.PublicKey); !ok {
			return SEPFailure(400, "INVALID_SIGNING_KEY", "签名公钥必须使用 Ed25519", "config")
		}
		key, keyErr = parseSEPPublicKey(c.EncryptionPublicKey)
		if keyErr != nil {
			return SEPFailure(400, "INVALID_ENCRYPTION_KEY", "凭证加密公钥格式无效", "config")
		}
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok || rsaKey.N.BitLen() < 3072 {
			return SEPFailure(400, "INVALID_ENCRYPTION_KEY", "凭证加密公钥必须使用至少 3072 位 RSA", "config")
		}
		for kind, id := range map[string]int64{"chat": c.ChatGroupID, "image": c.ImageGroupID, "video": c.VideoGroupID} {
			g, e := s.groups.GetByID(ctx, id)
			if e != nil || g == nil || !g.IsActive() || g.IsSubscriptionType() {
				return SEPFailure(400, "INVALID_SEP_GROUP", kind+" 必须选择启用的余额计费分组", "config")
			}
			if kind != "chat" && (g.Platform != PlatformOpenAI || !GroupAllowsImageGeneration(g)) {
				return SEPFailure(400, "INVALID_SEP_GROUP", kind+" 必须选择允许媒体生成的 OpenAI 平台分组", "config")
			}
		}
	}
	return s.settings.settingRepo.Set(ctx, SEPSettingKey, string(sepJSON(c)))
}
func (s *SEPService) Authenticate(ctx context.Context, r *http.Request, body []byte) (*SEPConfig, error) {
	c, err := s.Config(ctx)
	if err != nil {
		return nil, err
	}
	if !c.Enabled {
		return nil, SEPFailure(403, "SEP_INTEGRATION_DISABLED", "SEP 集成未启用", "authenticate")
	}
	ts, err := strconv.ParseInt(r.Header.Get("X-SEP-Timestamp"), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > 5*time.Minute || time.Until(time.Unix(ts, 0)) > time.Minute {
		return nil, SEPFailure(401, "INVALID_SERVICE_TIMESTAMP", "服务签名时间无效或已过期", "authenticate")
	}
	if r.Header.Get("X-SEP-Key-ID") != c.SigningKeyID || r.Header.Get("X-SEP-Version") != "1" || sepID(r.Header.Get("X-SEP-Nonce")) != nil {
		return nil, SEPFailure(401, "INVALID_SERVICE_AUTH", "服务身份或协议版本无效", "authenticate")
	}
	key, err := parseSEPPublicKey(c.SigningPublicKey)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(ed25519.PublicKey)
	sig, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-SEP-Signature"))
	if err != nil || !ok || !ed25519.Verify(pub, []byte(SEPCanonicalRequest(r, body)), sig) {
		return nil, SEPFailure(401, "INVALID_SERVICE_SIGNATURE", "SEP 服务签名校验失败", "authenticate")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO sep_request_nonces(integration_id,nonce,expires_at) VALUES($1,$2,NOW()+INTERVAL '10 minutes') ON CONFLICT DO NOTHING`, c.IntegrationID, r.Header.Get("X-SEP-Nonce"))
	if err != nil {
		return nil, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return nil, SEPFailure(401, "SERVICE_REPLAY_REJECTED", "服务签名已被使用，请用新签名恢复原业务编号", "authenticate")
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sep_request_nonces WHERE expires_at<NOW()`)
	return c, nil
}
func (s *SEPService) enterprise(ctx context.Context, integration, id string) (*SEPEnterprise, error) {
	e := &SEPEnterprise{IntegrationID: integration, BillingMode: "sep_settlement_v1", Credentials: []SEPCredential{}}
	err := s.db.QueryRowContext(ctx, `SELECT id,enterprise_id,user_id,company_name,admin_email,status FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2`, integration, id).Scan(&e.ID, &e.EnterpriseID, &e.ComputeAccountID, &e.CompanyName, &e.AdminEmail, &e.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	return e, err
}
func (s *SEPService) Enterprise(ctx context.Context, integration, id string, delivery bool) (*SEPEnterprise, error) {
	e, err := s.enterprise(ctx, integration, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.kind,c.id,c.version,k.group_id,c.encryption_key_id,c.masked_key,c.encrypted_key FROM sep_credentials c JOIN api_keys k ON k.id=c.api_key_id WHERE enterprise_pk=$1 AND c.is_current ORDER BY c.kind`, e.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c SEPCredential
		c.DeliveryStatus = "available"
		if err = rows.Scan(&c.Type, &c.CredentialID, &c.Version, &c.GroupID, &c.EncryptionKeyID, &c.MaskedKey, &c.EncryptedKey); err != nil {
			return nil, err
		}
		if !delivery {
			c.EncryptedKey = ""
		}
		e.Credentials = append(e.Credentials, c)
	}
	return e, rows.Err()
}
func (s *SEPService) Register(ctx context.Context, cfg *SEPConfig, r SEPRegistration) (*SEPEnterprise, error) {
	if err := sepID(r.EnterpriseID); err != nil {
		return nil, err
	}
	if err := sepID(r.RegistrationRef); err != nil {
		return nil, err
	}
	addr, err := mail.ParseAddress(r.AdminEmail)
	if err != nil || addr.Address != r.AdminEmail || len(r.AdminEmail) > 254 || strings.TrimSpace(r.CompanyName) == "" || len(r.CompanyName) > 200 {
		return nil, SEPFailure(400, "INVALID_ENTERPRISE_IDENTITY", "企业名称或管理员邮箱格式无效", "provision")
	}
	fingerprint := sepHash(sepJSON(r))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Both enterprise identity and registration id are serialized across all server instances.
	for _, v := range []string{"sep-register:" + cfg.IntegrationID, "sep-enterprise:" + r.EnterpriseID} {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, v); err != nil {
			return nil, err
		}
	}
	var previousHash, previousID string
	err = tx.QueryRowContext(ctx, `SELECT registration_hash,enterprise_id FROM sep_enterprises WHERE integration_id=$1 AND (enterprise_id=$2 OR registration_ref=$3)`, cfg.IntegrationID, r.EnterpriseID, r.RegistrationRef).Scan(&previousHash, &previousID)
	if err == nil {
		if previousHash != fingerprint || previousID != r.EnterpriseID {
			return nil, sepConflict("IDEMPOTENCY_CONFLICT", "企业或注册编号已被其他注册内容使用")
		}
		tx.Rollback()
		return s.Enterprise(ctx, cfg.IntegrationID, r.EnterpriseID, true)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if r.EncryptionKeyID != cfg.EncryptionKeyID {
		return nil, sepConflict("ENCRYPTION_KEY_VERSION_MISMATCH", "凭证加密公钥版本不匹配")
	}
	if _, err := bcrypt.Cost([]byte(r.PasswordHash)); err != nil || len(r.PasswordHash) != 60 {
		return nil, SEPFailure(400, "INVALID_PASSWORD_HASH", "开户需要有效的 bcrypt 密码哈希", "provision")
	}
	var userID, enterprisePK int64
	err = tx.QueryRowContext(ctx, `INSERT INTO users(email,password_hash,role,balance,frozen_balance,concurrency,status,username) VALUES($1,$2,'user',0,0,5,'active',$3) RETURNING id`, r.AdminEmail, r.PasswordHash, r.CompanyName).Scan(&userID)
	if err != nil {
		var conflict *pq.Error
		if errors.As(err, &conflict) && conflict.Code == "23505" {
			return nil, sepConflict("EMAIL_ALREADY_EXISTS", "该邮箱已在算力平台注册，不能覆盖或自动绑定已有账户")
		}
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO sep_enterprises(integration_id,enterprise_id,registration_ref,registration_hash,user_id,company_name,admin_email) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`, cfg.IntegrationID, r.EnterpriseID, r.RegistrationRef, fingerprint, userID, r.CompanyName, r.AdminEmail).Scan(&enterprisePK)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL sep.key_management='on'`); err != nil {
		return nil, err
	}
	public, err := parseSEPPublicKey(cfg.EncryptionPublicKey)
	if err != nil {
		return nil, err
	}
	enc, err := jose.NewEncrypter(jose.A256GCM, jose.Recipient{Algorithm: jose.RSA_OAEP_256, Key: public, KeyID: cfg.EncryptionKeyID}, nil)
	if err != nil {
		return nil, err
	}
	for _, kind := range []string{"chat", "image", "video"} {
		gid := map[string]int64{"chat": cfg.ChatGroupID, "image": cfg.ImageGroupID, "video": cfg.VideoGroupID}[kind]
		var valid bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id=$1 AND status='active' AND deleted_at IS NULL AND subscription_type='standard')`, gid).Scan(&valid); err != nil {
			return nil, err
		}
		if !valid {
			return nil, sepConflict("INVALID_SEP_GROUP", "开户默认分组已不可用")
		}
		key := sepRandom("sk-sep-")
		cid := sepRandom("cred_")
		var keyID int64
		err = tx.QueryRowContext(ctx, `INSERT INTO api_keys(user_id,key,name,group_id,status) VALUES($1,$2,$3,$4,'active') RETURNING id`, userID, key, "SEP "+kind, gid).Scan(&keyID)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, userID, gid); err != nil {
			return nil, err
		}
		payload := map[string]any{"enterprise_id": r.EnterpriseID, "compute_account_id": userID, "type": kind, "credential_id": cid, "credential_version": 1, "key": key}
		jwe, e := enc.Encrypt(sepJSON(payload))
		if e != nil {
			return nil, e
		}
		sealed, e := jwe.CompactSerialize()
		if e != nil {
			return nil, e
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sep_credentials(id,enterprise_pk,kind,api_key_id,encryption_key_id,masked_key,encrypted_key) VALUES($1,$2,$3,$4,$5,$6,$7)`, cid, enterprisePK, kind, keyID, cfg.EncryptionKeyID, key[:10]+"..."+key[len(key)-4:], sealed)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Enterprise(ctx, cfg.IntegrationID, r.EnterpriseID, true)
}
func (s *SEPService) Balance(ctx context.Context, integration, id string) (*SEPBalance, error) {
	b := &SEPBalance{EnterpriseID: id, Currency: "CNY", ObservedAt: time.Now().UTC()}
	err := s.db.QueryRowContext(ctx, `SELECT (available_cny+frozen_cny)::text,available_cny::text,frozen_cny::text,sequence::text FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2`, integration, id).Scan(&b.Total, &b.Available, &b.Frozen, &b.Sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sepMissing()
	}
	return b, err
}
func (s *SEPService) ListEnterprises(ctx context.Context, limit int) ([]SEPEnterprise, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,integration_id,enterprise_id,user_id,company_name,admin_email,status FROM sep_enterprises ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SEPEnterprise{}
	for rows.Next() {
		var e SEPEnterprise
		if err = rows.Scan(&e.ID, &e.IntegrationID, &e.EnterpriseID, &e.ComputeAccountID, &e.CompanyName, &e.AdminEmail, &e.Status); err != nil {
			return nil, err
		}
		e.BillingMode = "sep_settlement_v1"
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *SEPService) ManageEnterprise(ctx context.Context, integration, id, status string, groups map[string]int64) error {
	if status != "ready" && status != "disabled" {
		return SEPFailure(400, "INVALID_STATUS", "企业状态无效", "manage")
	}
	e, err := s.enterprise(ctx, integration, id)
	if err != nil {
		return err
	}
	for kind, gid := range groups {
		if kind != "chat" && kind != "image" && kind != "video" {
			return SEPFailure(400, "INVALID_KEY_TYPE", "Key 用途无效", "manage")
		}
		g, e := s.groups.GetByID(ctx, gid)
		if e != nil || g == nil || !g.IsActive() || g.IsSubscriptionType() || (kind != "chat" && (g.Platform != PlatformOpenAI || !GroupAllowsImageGeneration(g))) {
			return SEPFailure(400, "INVALID_SEP_GROUP", "请选择启用的余额计费分组，媒体需选择允许媒体生成的 OpenAI 平台分组", "manage")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE sep_enterprises SET status=$1 WHERE id=$2`, status, e.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL sep.key_management='on'`); err != nil {
		return err
	}
	for kind, gid := range groups {
		var rotating bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_credential_rotations WHERE enterprise_pk=$1 AND kind=$2 AND status<>'completed')`, e.ID, kind).Scan(&rotating); err != nil {
			return err
		}
		if rotating {
			return sepConflict("CREDENTIAL_ROTATION_IN_PROGRESS", "该用途凭证正在更换，请完成原更换后再调整分组")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET group_id=$1,updated_at=NOW() WHERE id=(SELECT api_key_id FROM sep_credentials WHERE enterprise_pk=$2 AND kind=$3 AND is_current)`, gid, e.ID, kind); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, e.ComputeAccountID, gid); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.keys.InvalidateAuthCacheByUserID(ctx, e.ComputeAccountID)
	return nil
}

func (s *APIKeyService) SEPService() *SEPService { return s.sep }
func (s *SEPService) IsEnterpriseUser(ctx context.Context, userID int64) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sep_enterprises WHERE user_id=$1)`, userID).Scan(&found)
	return found, err
}
