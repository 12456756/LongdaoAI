package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/errordetail"
)

type SEPBalanceEvent struct {
	EventID       string          `json:"event_id"`
	Sequence      string          `json:"sequence"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	LastError     string          `json:"last_error"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
}

func validateSEPWebhook(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return SEPFailure(400, "INVALID_BALANCE_WEBHOOK_URL", "余额通知地址必须是完整 URL，不能包含用户名、密码或片段", "config")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))) {
		return SEPFailure(400, "INVALID_BALANCE_WEBHOOK_URL", "余额通知地址必须使用 HTTPS；本机测试仅允许回环地址 HTTP", "config")
	}
	return nil
}
func sepEventSigningKey() (ed25519.PrivateKey, string, error) {
	raw := strings.TrimSpace(os.Getenv("SEP_EVENT_SIGNING_PRIVATE_KEY"))
	keyID := strings.TrimSpace(os.Getenv("SEP_EVENT_SIGNING_KEY_ID"))
	if keyID == "" {
		keyID = "compute-events-v1"
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, keyID, SEPFailure(503, "BALANCE_SIGNING_KEY_MISSING", "余额通知签名私钥未配置：SEP_EVENT_SIGNING_PRIVATE_KEY 必须为 Base64 编码的 32 字节 Ed25519 种子", "notify")
	}
	return ed25519.NewKeyFromSeed(seed), keyID, nil
}
func SEPEventPublicKey() map[string]string {
	key, id, err := sepEventSigningKey()
	if err != nil {
		return map[string]string{"key_id": id, "public_key": "", "error": err.Error()}
	}
	der, _ := x509.MarshalPKIXPublicKey(key.Public())
	return map[string]string{"key_id": id, "public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), "algorithm": "Ed25519"}
}
func (s *SEPService) BalanceEvents(ctx context.Context, integration, enterprise string, after int64, limit int) (map[string]any, error) {
	if after < 0 || limit < 1 || limit > 200 {
		return nil, SEPFailure(400, "INVALID_EVENT_CURSOR", "同步事件游标必须非负，单页条数须在 1 至 200 之间", "events")
	}
	var pk int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM sep_enterprises WHERE integration_id=$1 AND enterprise_id=$2`, integration, enterprise).Scan(&pk); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sepMissing()
		}
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id,sequence::text,payload,status,attempts,last_error,next_attempt_at FROM sep_balance_events WHERE enterprise_pk=$1 AND sequence>$2 ORDER BY sep_balance_events.sequence ASC LIMIT $3`, pk, after, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SEPBalanceEvent{}
	for rows.Next() {
		var e SEPBalanceEvent
		if err = rows.Scan(&e.EventID, &e.Sequence, &e.Payload, &e.Status, &e.Attempts, &e.LastError, &e.NextAttemptAt); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	next := fmt.Sprint(after)
	if len(items) > 0 {
		next = items[len(items)-1].Sequence
	}
	return map[string]any{"contract_version": 1, "items": items, "after_sequence": next, "has_more": more}, nil
}
func (s *SEPService) AcknowledgeBalanceEvent(ctx context.Context, integration, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sep_balance_events v SET status='delivered',delivered_at=COALESCE(delivered_at,NOW()),last_error='' FROM sep_enterprises e WHERE v.enterprise_pk=e.id AND e.integration_id=$1 AND v.event_id=$2`, integration, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sepMissing()
	}
	return nil
}
func (s *SEPService) RetryBalanceEvent(ctx context.Context, integration, id string) error {
	cfg, err := s.Config(ctx)
	if err != nil {
		return err
	}
	if cfg.BalanceWebhookURL == "" {
		return SEPFailure(409, "BALANCE_WEBHOOK_NOT_CONFIGURED", "尚未配置 SEP 余额通知接收地址；事件已保存，可由 SEP 补拉，未重复执行账务", "notify")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE sep_balance_events v SET status='pending',next_attempt_at=NOW() FROM sep_enterprises e WHERE v.enterprise_pk=e.id AND e.integration_id=$1 AND v.event_id=$2 AND v.status<>'delivered'`, integration, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sepConflict("EVENT_NOT_RETRYABLE", "同步事件不存在或已送达，无需重复重试")
	}
	return nil
}
func (s *SEPService) StartBalanceSync() {
	s.syncOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.syncCancel = cancel
		s.syncDone = make(chan struct{})
		go func() {
			defer close(s.syncDone)
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					work, cancel := context.WithTimeout(ctx, 25*time.Second)
					if err := s.retryPendingSettlements(work); err != nil {
						slog.Error("sep pending settlement retry failed", "error", errordetail.Text(err.Error()))
					}
					if err := s.retryDirectBills(work); err != nil {
						slog.Error("sep direct settlement retry failed", "error", errordetail.Text(err.Error()))
					}
					if err := s.deliverBalanceEvents(work); err != nil {
						slog.Error("sep balance event dispatch failed", "error", errordetail.Text(err.Error()))
					}
					cancel()
				}
			}
		}()
	})
}
func (s *SEPService) StopBalanceSync() {
	if s.syncCancel != nil {
		s.syncCancel()
		<-s.syncDone
	}
}
func (s *SEPService) deliverBalanceEvents(ctx context.Context) error {
	cfg, err := s.Config(ctx)
	if err != nil {
		return err
	}
	if cfg.BalanceWebhookURL == "" {
		return nil
	}
	if err = validateSEPWebhook(cfg.BalanceWebhookURL); err != nil {
		return err
	}
	// Only the earliest unacknowledged event for each enterprise may be delivered.
	rows, err := s.db.QueryContext(ctx, `WITH picked AS (SELECT v.event_id FROM sep_balance_events v JOIN sep_enterprises e ON e.id=v.enterprise_pk WHERE e.integration_id=$1 AND v.status<>'delivered' AND v.next_attempt_at<=NOW() AND NOT EXISTS(SELECT 1 FROM sep_balance_events p WHERE p.enterprise_pk=v.enterprise_pk AND p.sequence<v.sequence AND p.status<>'delivered') ORDER BY v.created_at FOR UPDATE OF v SKIP LOCKED LIMIT 10) UPDATE sep_balance_events v SET status='retrying',attempts=attempts+1,next_attempt_at=NOW()+INTERVAL '60 seconds' FROM picked WHERE v.event_id=picked.event_id RETURNING v.event_id,v.payload,v.attempts`, cfg.IntegrationID)
	if err != nil {
		return err
	}
	type pending struct {
		id       string
		payload  []byte
		attempts int
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err = rows.Scan(&p.id, &p.payload, &p.attempts); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range batch {
		err = s.sendBalanceEvent(ctx, cfg.BalanceWebhookURL, p.id, p.payload)
		if err == nil {
			if err = s.AcknowledgeBalanceEvent(ctx, cfg.IntegrationID, p.id); err != nil {
				return err
			}
			continue
		}
		delay := time.Duration(1<<min(p.attempts, 9)) * time.Second
		detail := errordetail.Text(err.Error())
		chars := []rune(detail)
		if len(chars) > 1000 {
			detail = string(chars[:1000])
		}
		if _, saveErr := s.db.ExecContext(ctx, `UPDATE sep_balance_events SET status='failed',last_error=$1,next_attempt_at=NOW()+$2*INTERVAL '1 second' WHERE event_id=$3 AND status<>'delivered'`, detail, int(delay.Seconds()), p.id); saveErr != nil {
			return saveErr
		}
	}
	return nil
}
func (s *SEPService) sendBalanceEvent(ctx context.Context, target, id string, payload []byte) error {
	key, keyID, err := sepEventSigningKey()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	canonical := fmt.Sprintf("compute-balance-event-v1\n%s\n%s\n%s\nPOST\n%s\n%s", keyID, now, nonceText, req.URL.RequestURI(), sepHash(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Compute-Key-ID", keyID)
	req.Header.Set("X-Compute-Timestamp", now)
	req.Header.Set("X-Compute-Nonce", nonceText)
	req.Header.Set("X-Compute-Signature", base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(canonical))))
	req.Header.Set("Idempotency-Key", id)
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("事件 %s 通知失败：%s", id, errordetail.Text(err.Error()))
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return fmt.Errorf("事件 %s 通知响应读取失败：%w", id, err)
	}
	var ack struct {
		EventID string `json:"event_id"`
		Status  string `json:"status"`
	}
	decodeErr := json.Unmarshal(raw, &ack)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("事件 %s 通知被 SEP 拒绝：HTTP %d，%s", id, response.StatusCode, errordetail.Text(strings.TrimSpace(string(raw))))
	}
	if decodeErr != nil || ack.EventID != id || ack.Status != "applied" {
		return fmt.Errorf("事件 %s 未获入账确认：SEP 响应须包含同一 event_id 和 status=applied，实际响应 %s", id, errordetail.Text(strings.TrimSpace(string(raw))))
	}
	return nil
}
