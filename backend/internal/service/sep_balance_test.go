package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type sepBalanceSettings struct {
	SettingRepository
	cfg  SEPConfig
	rate string
}

func (s *sepBalanceSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{SettingKeyPlatformUSDToCNYRate: s.rate}, nil
}
func (s *sepBalanceSettings) GetValue(context.Context, string) (string, error) {
	return string(sepJSON(s.cfg)), nil
}

func balanceTestService(t *testing.T) (*SEPService, *sql.DB, *sepBalanceSettings) {
	t.Helper()
	dsn := os.Getenv("TEST_SEP_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set TEST_SEP_DATABASE_DSN to an isolated PostgreSQL database")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("sep_balance_%d", time.Now().UnixNano())
	_, err = db.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	parsed, err := url.Parse(dsn)
	require.NoError(t, err)
	params := parsed.Query()
	params.Set("search_path", schema)
	parsed.RawQuery = params.Encode()
	db, err = sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(8)
	t.Cleanup(func() {
		_, err := db.Exec("DROP SCHEMA " + schema + " CASCADE")
		require.NoError(t, err)
		require.NoError(t, db.Close())
	})
	_, err = db.Exec("SET search_path TO " + schema)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE users(id BIGINT PRIMARY KEY,balance NUMERIC DEFAULT 0,frozen_balance NUMERIC DEFAULT 0,password_hash TEXT,email TEXT,role TEXT,deleted_at TIMESTAMPTZ);
CREATE TABLE api_keys(id BIGINT PRIMARY KEY,user_id BIGINT,key TEXT,group_id BIGINT,quota NUMERIC,rate_limit_5h NUMERIC,rate_limit_1d NUMERIC,rate_limit_7d NUMERIC);
CREATE TABLE accounts(id BIGINT PRIMARY KEY);
CREATE TABLE payment_orders(id BIGINT PRIMARY KEY,user_id BIGINT);
CREATE TABLE redeem_codes(id BIGINT PRIMARY KEY,type TEXT,status TEXT,used_by BIGINT,used_at TIMESTAMPTZ,expires_at TIMESTAMPTZ);
INSERT INTO users(id) VALUES(1),(2); INSERT INTO accounts VALUES(1);`)
	require.NoError(t, err)
	for _, name := range []string{"185_sep_enterprise_settlement.sql", "190_sep_ledger_entry_types.sql", "192_sep_balance_events.sql", "195_sep_pending_settlements.sql", "196_sep_credential_rotations.sql"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		require.NoError(t, err)
		_, err = db.Exec(string(raw))
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO sep_enterprises(id,integration_id,enterprise_id,registration_ref,registration_hash,user_id,company_name,admin_email) VALUES(1,'sep-test','ent-test','reg-test','hash',1,'Company','test@example.test'); INSERT INTO api_keys(id,user_id) VALUES(1,2); INSERT INTO payment_orders VALUES(1,1);`)
	require.NoError(t, err)
	settings := &sepBalanceSettings{rate: "7.15", cfg: SEPConfig{Enabled: true, IntegrationID: "sep-test"}}
	return &SEPService{db: db, settings: &SettingService{settingRepo: settings}}, db, settings
}
func TestSEPBalanceAtomicEventsAndIdempotency(t *testing.T) {
	svc, db, settings := balanceTestService(t)
	change := SEPBalanceChange{UserID: 1, BusinessRef: "admin-1", Operation: "add", Amount: "10", Currency: "CNY", Notes: "manual recharge", ActorID: 7}
	receipt, err := svc.AdjustAccountBalance(t.Context(), change)
	require.NoError(t, err)
	require.Equal(t, "10.000000", receipt.Available)
	settings.rate = "8"
	replay, err := svc.AdjustAccountBalance(t.Context(), change)
	require.NoError(t, err)
	require.Equal(t, receipt, replay)
	changed := change
	changed.Amount = "11"
	_, err = svc.AdjustAccountBalance(t.Context(), changed)
	require.ErrorContains(t, err, "BALANCE_OPERATION_CONFLICT")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_balance_events`).Scan(&count))
	require.Equal(t, 1, count)
	var payload map[string]any
	var raw []byte
	require.NoError(t, db.QueryRow(`SELECT payload FROM sep_balance_events`).Scan(&raw))
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, "compute", payload["origin"])
	require.Equal(t, "10.000000", payload["delta_cny"])
	change.BusinessRef = "subtract"
	change.Operation = "subtract"
	change.Amount = "12"
	_, err = svc.AdjustAccountBalance(t.Context(), change)
	require.ErrorContains(t, err, "可用余额 10.000000 元")
	change.BusinessRef = "set"
	change.Operation = "set"
	change.Amount = "5"
	receipt, err = svc.AdjustAccountBalance(t.Context(), change)
	require.NoError(t, err)
	require.Equal(t, "-5.000000", receipt.Delta)
	// Every ledger event is mandatory: insertion failure must roll back the balance.
	_, err = db.Exec(`ALTER TABLE sep_balance_events ADD CONSTRAINT test_block CHECK(false) NOT VALID`)
	require.NoError(t, err)
	change.BusinessRef = "rollback"
	change.Operation = "add"
	change.Amount = "1"
	_, err = svc.AdjustAccountBalance(t.Context(), change)
	require.Error(t, err)
	var balance string
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "5.000000", balance)
	_, err = db.Exec(`ALTER TABLE sep_balance_events DROP CONSTRAINT test_block`)
	require.NoError(t, err)
	// Ordinary users still use their original dollar column; enterprise direct writes remain protected.
	_, err = db.Exec(`UPDATE users SET balance=3 WHERE id=2`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE users SET balance=3 WHERE id=1`)
	require.ErrorContains(t, err, "SEP_ACCOUNT_MANAGED")
	change.BusinessRef = "redeem"
	change.Currency = "USD"
	change.Amount = "1"
	change.RedeemCodeID = 1
	_, err = db.Exec(`INSERT INTO redeem_codes(id,type,status) VALUES(1,'balance','unused')`)
	require.NoError(t, err)
	receipt, err = svc.AdjustAccountBalance(t.Context(), change)
	require.NoError(t, err)
	require.Equal(t, "13.000000", receipt.Available)
	var state string
	require.NoError(t, db.QueryRow(`SELECT status FROM redeem_codes WHERE id=1`).Scan(&state))
	require.Equal(t, "used", state)
	change.RedeemCodeID = 0
	for i := 0; i < 10; i++ {
		change.BusinessRef = fmt.Sprintf("extra-%d", i)
		change.Amount = "0.01"
		_, err = svc.AdjustAccountBalance(t.Context(), change)
		require.NoError(t, err)
	}
	var after int64
	var seen []string
	for {
		result, err := svc.BalanceEvents(t.Context(), "sep-test", "ent-test", after, 3)
		require.NoError(t, err)
		for _, e := range result["items"].([]SEPBalanceEvent) {
			seen = append(seen, e.Sequence)
			fmt.Sscan(e.Sequence, &after)
		}
		if !result["has_more"].(bool) {
			break
		}
	}
	require.Len(t, seen, 13)
	for i, n := range seen {
		require.Equal(t, fmt.Sprint(i+1), n)
	}
	err = svc.AcknowledgeBalanceEvent(t.Context(), "other", receipt.EventID)
	require.ErrorContains(t, err, "SEP_RESOURCE_NOT_FOUND")
	require.NoError(t, svc.AcknowledgeBalanceEvent(t.Context(), "sep-test", receipt.EventID))
	require.NoError(t, svc.AcknowledgeBalanceEvent(t.Context(), "sep-test", receipt.EventID))
}
func TestSEPDirectSettlementAndRefundRecovery(t *testing.T) {
	svc, db, _ := balanceTestService(t)
	_, err := svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "fund", Operation: "add", Amount: "10", Currency: "CNY"})
	require.NoError(t, err)
	price := &ModelBasePrice{Model: "image", Price: AccountModelPrice{Mode: BillingModeImage}}
	e := &SEPExecution{ComputeRequestID: "compute-direct", EnterprisePK: 1, EnterpriseID: "ent-test", ExecutionID: "run-1", Rate: "7.15", Kind: "image", Price: price, DirectBilling: true}
	_, err = db.Exec(`INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,billing_origin) VALUES('compute-direct','sep-test',1,'run-1',1,'image','image','hash',7.15,'{}','{}','compute')`)
	require.NoError(t, err)
	cmd := &UsageBillingCommand{APIKeyID: 1, UserID: 1, AccountID: 1, BalanceCost: 0.2}
	result, err := svc.FinalizeUsage(t.Context(), e, cmd, nil)
	require.NoError(t, err)
	require.True(t, result.Applied)
	readCommittedAt := func(execution string) string {
		t.Helper()
		bill, err := svc.Billing(t.Context(), "sep-test", execution)
		require.NoError(t, err)
		require.Equal(t, "committed", bill["settlement_status"])
		receipt := bill["settlement_receipt"].(map[string]any)
		committed, ok := receipt["committed_at"].(string)
		require.True(t, ok, "committed_at must be an RFC3339 timestamp")
		_, err = time.Parse(time.RFC3339Nano, committed)
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(committed, "Z"))
		return committed
	}
	committedAt := readCommittedAt("run-1")
	result, err = svc.FinalizeUsage(t.Context(), e, cmd, nil)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, committedAt, readCommittedAt("run-1"))
	b, err := svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "8.570000", b.Available)
	_, err = svc.Settle(t.Context(), "sep-test", "run-1", SEPSettlement{ContractVersion: 1, EnterpriseID: "ent-test", SettlementID: "sep-double", BillID: "bill", BillVersion: 1, Currency: "CNY", GrossCNY: "1.430000", FreeCNY: "0.000000", PaidCNY: "1.430000"})
	require.ErrorContains(t, err, "DIRECT_EXECUTION_ALREADY_BILLED")
	_, err = svc.RefundBalance(t.Context(), 1, "deduct", "2.000000")
	require.NoError(t, err)
	_, err = svc.RefundBalance(t.Context(), 1, "deduct", "2.000000")
	require.NoError(t, err)
	b, err = svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "6.570000", b.Available)
	_, err = svc.RefundBalance(t.Context(), 1, "restore", "")
	require.NoError(t, err)
	_, err = svc.RefundBalance(t.Context(), 1, "restore", "")
	require.NoError(t, err)
	b, err = svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "8.570000", b.Available)
	_, err = svc.RefundBalance(t.Context(), 1, "deduct", "3.000000")
	require.NoError(t, err)
	_, err = svc.RefundBalance(t.Context(), 1, "complete", "")
	require.NoError(t, err)
	_, err = svc.RefundBalance(t.Context(), 1, "restore", "")
	require.NoError(t, err)
	b, err = svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "5.570000", b.Available)
	// A final fee larger than the balance remains payable and is settled only once after recharge.
	_, err = db.Exec(`INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,billing_origin) VALUES('compute-unpaid','sep-test',1,'run-2',1,'image','image','hash',7.15,'{}','{}','compute')`)
	require.NoError(t, err)
	e.ComputeRequestID = "compute-unpaid"
	e.ExecutionID = "run-2"
	cmd.BalanceCost = 1
	_, err = svc.FinalizeUsage(t.Context(), e, cmd, nil)
	require.NoError(t, err)
	var problem string
	require.NoError(t, db.QueryRow(`SELECT billing_error FROM sep_executions WHERE id='compute-unpaid'`).Scan(&problem))
	require.Contains(t, problem, "尚未扣款")
	unpaid, err := svc.Billing(t.Context(), "sep-test", "run-2")
	require.NoError(t, err)
	require.Equal(t, "unsettled", unpaid["settlement_status"])
	require.Nil(t, unpaid["settlement_receipt"])
	_, err = svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "repay", Operation: "add", Amount: "10", Currency: "CNY"})
	require.NoError(t, err)
	require.NoError(t, svc.retryDirectBills(t.Context()))
	committedAt = readCommittedAt("run-2")
	require.NoError(t, svc.retryDirectBills(t.Context()))
	require.Equal(t, committedAt, readCommittedAt("run-2"))
	b, err = svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "8.420000", b.Available)
	// Existing SEP settlements still split free and paid portions without touching dollar balances.
	_, err = db.Exec(`INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,fee_status,gross_cny,bill_id,bill_version) VALUES('compute-sep','sep-test',1,'run-sep',1,'chat','chat','hash',7.15,'{}','{}','final',3,'bill-sep',1)`)
	require.NoError(t, err)
	settlement := SEPSettlement{ContractVersion: 1, EnterpriseID: "ent-test", SettlementID: "settle-sep", BillID: "bill-sep", BillVersion: 1, Currency: "CNY", GrossCNY: "3.000000", FreeCNY: "2.000000", PaidCNY: "1.000000"}
	_, err = svc.Settle(t.Context(), "sep-test", "run-sep", settlement)
	require.NoError(t, err)
	_, err = svc.Settle(t.Context(), "sep-test", "run-sep", settlement)
	require.NoError(t, err)
	b, err = svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "7.420000", b.Available)
}

func TestSEPDirectReceiptCommittedAtMigration(t *testing.T) {
	svc, db, _ := balanceTestService(t)
	_, err := db.Exec(`INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,billing_origin)
SELECT 'migration-'||n,'sep-test',1,'migration-'||n,1,'chat','chat','hash',7.15,'{}','{}',CASE WHEN n=4 THEN 'sep' ELSE 'compute' END FROM generate_series(1,5) n;
INSERT INTO sep_settlements(integration_id,settlement_id,execution_pk,request_hash,gross_cny,free_cny,paid_cny,receipt,created_at)
SELECT 'sep-test','migration-'||n,'migration-'||n,'hash',1,0,1,
    '{"status":"committed","marker":"preserved"}'::jsonb || CASE n
        WHEN 2 THEN '{"committed_at":null}'::jsonb
        WHEN 3 THEN '{"committed_at":"2026-11-20T01:02:03Z"}'::jsonb
        WHEN 5 THEN '{"committed_at":""}'::jsonb
        ELSE '{}'::jsonb END,
    '2026-11-20T16:00:01.123456+08:00'::timestamptz
FROM generate_series(1,5) n`)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "193_sep_direct_receipt_committed_at.sql"))
	require.NoError(t, err)
	for pass := 0; pass < 2; pass++ {
		result, err := db.Exec(string(raw))
		require.NoError(t, err)
		changed, err := result.RowsAffected()
		require.NoError(t, err)
		if pass == 0 {
			require.EqualValues(t, 3, changed)
		} else {
			require.Zero(t, changed, "migration replay must preserve saved timestamps")
		}
		for _, n := range []int{1, 2, 5} {
			bill, err := svc.Billing(t.Context(), "sep-test", fmt.Sprintf("migration-%d", n))
			require.NoError(t, err)
			require.Equal(t, "committed", bill["settlement_status"])
			receipt := bill["settlement_receipt"].(map[string]any)
			require.Equal(t, "2026-11-20T08:00:01.123456Z", receipt["committed_at"])
			require.Equal(t, "preserved", receipt["marker"])
		}
	}
	var stored string
	require.NoError(t, db.QueryRow(`SELECT receipt->>'committed_at' FROM sep_settlements WHERE settlement_id='migration-3'`).Scan(&stored))
	require.Equal(t, "2026-11-20T01:02:03Z", stored)
	var untouched bool
	require.NoError(t, db.QueryRow(`SELECT NOT receipt ? 'committed_at' FROM sep_settlements WHERE settlement_id='migration-4'`).Scan(&untouched))
	require.True(t, untouched, "SEP-origin receipts are outside this repair")
	var balance string
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "0.000000", balance)
	for _, table := range []string{"sep_ledger", "sep_balance_events"} {
		var count int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count, "receipt repair must not create accounting records")
	}
}

func TestSEPEventDeliveryRetriesInOrderWithoutRepeatingBalance(t *testing.T) {
	svc, db, settings := balanceTestService(t)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	t.Setenv("SEP_EVENT_SIGNING_PRIVATE_KEY", base64.StdEncoding.EncodeToString(private.Seed()))
	fail := true
	var delivered []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := body["event_id"].(string)
		if fail {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"message":"enterprise binding missing"}`)
			return
		}
		delivered = append(delivered, body["sequence"].(string))
		json.NewEncoder(w).Encode(map[string]string{"event_id": id, "status": "applied"})
	}))
	defer server.Close()
	settings.cfg.BalanceWebhookURL = server.URL
	for i := 0; i < 3; i++ {
		_, err = svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: fmt.Sprint(i), Operation: "add", Amount: "1", Currency: "CNY"})
		require.NoError(t, err)
	}
	require.NoError(t, svc.deliverBalanceEvents(t.Context()))
	var reason, status string
	var attempts int
	require.NoError(t, db.QueryRow(`SELECT status,last_error,attempts FROM sep_balance_events WHERE sequence=1`).Scan(&status, &reason, &attempts))
	require.Equal(t, "failed", status)
	require.Contains(t, reason, "HTTP 409")
	require.Contains(t, reason, "enterprise binding missing")
	require.Equal(t, 1, attempts)
	require.NoError(t, db.QueryRow(`SELECT attempts FROM sep_balance_events WHERE sequence=2`).Scan(&attempts))
	require.Zero(t, attempts)
	fail = false
	_, err = db.Exec(`UPDATE sep_balance_events SET next_attempt_at=NOW()`)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		require.NoError(t, svc.deliverBalanceEvents(t.Context()))
	}
	require.Equal(t, []string{"1", "2", "3"}, delivered)
	b, err := svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "3.000000", b.Available)
}
func TestSEPBalanceWebhookSignatureAndAcknowledgement(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	t.Setenv("SEP_EVENT_SIGNING_PRIVATE_KEY", base64.StdEncoding.EncodeToString(private.Seed()))
	t.Setenv("SEP_EVENT_SIGNING_KEY_ID", "test-events")
	var mu sync.Mutex
	var problem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		canonical := fmt.Sprintf("compute-balance-event-v1\n%s\n%s\n%s\nPOST\n%s\n%s", r.Header.Get("X-Compute-Key-ID"), r.Header.Get("X-Compute-Timestamp"), r.Header.Get("X-Compute-Nonce"), r.URL.RequestURI(), sepHash(raw))
		signature, _ := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Compute-Signature"))
		mu.Lock()
		if !ed25519.Verify(private.Public().(ed25519.PublicKey), []byte(canonical), signature) {
			problem = "invalid signature"
		}
		mu.Unlock()
		if r.URL.Path == "/reject" {
			w.WriteHeader(409)
			fmt.Fprint(w, `{"message":"event amount mismatch password=secret"}`)
			return
		}
		if r.URL.Path == "/missing" {
			fmt.Fprint(w, `{"status":"received"}`)
			return
		}
		fmt.Fprint(w, `{"event_id":"event-1","status":"applied"}`)
	}))
	defer server.Close()
	svc := &SEPService{}
	require.NoError(t, svc.sendBalanceEvent(t.Context(), server.URL+"/accept", "event-1", []byte(`{"delta_cny":"1.000000"}`)))
	err = svc.sendBalanceEvent(t.Context(), server.URL+"/reject", "event-1", []byte(`{}`))
	require.ErrorContains(t, err, "HTTP 409")
	require.ErrorContains(t, err, "event amount mismatch")
	require.NotContains(t, err.Error(), "password=secret")
	err = svc.sendBalanceEvent(t.Context(), server.URL+"/missing", "event-1", []byte(`{}`))
	require.ErrorContains(t, err, "未获入账确认")
	mu.Lock()
	require.Empty(t, problem)
	mu.Unlock()
	require.NoError(t, validateSEPWebhook(server.URL))
	require.Error(t, validateSEPWebhook("http://external.example/events"))
	require.Error(t, validateSEPWebhook("https://user:secret@example.com/events"))
	require.NotEmpty(t, SEPEventPublicKey()["public_key"])
	t.Setenv("SEP_EVENT_SIGNING_PRIVATE_KEY", "")
	_, _, err = sepEventSigningKey()
	require.ErrorContains(t, err, "BALANCE_SIGNING_KEY_MISSING")
	require.False(t, strings.Contains(SEPEventPublicKey()["error"], base64.StdEncoding.EncodeToString(private.Seed())))
}

func TestSEPBalanceConcurrentDebitsCannotOverspend(t *testing.T) {
	svc, db, _ := balanceTestService(t)
	_, err := svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "fund", Operation: "add", Amount: "5", Currency: "CNY"})
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: fmt.Sprintf("debit-%d", i), Operation: "subtract", Amount: "1", Currency: "CNY"})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			require.ErrorContains(t, err, "INSUFFICIENT_PAID_FUNDS")
		}
	}
	require.Equal(t, 5, success)
	b, err := svc.AccountBalance(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "0.000000", b.Available)
	var events int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_balance_events`).Scan(&events))
	require.Equal(t, 6, events)
}

func TestSEPDirectBillingRequiresExplicitModeAndKeepsSignedFailures(t *testing.T) {
	for _, test := range []struct {
		mode, sepHeader string
		enabled         bool
		want            string
	}{
		{mode: "direct", enabled: false, want: "DIRECT_BILLING_DISABLED"},
		{mode: "direct", sepHeader: "bad-signature", enabled: true, want: "BILLING_ORIGIN_CONFLICT"},
		{mode: "", enabled: true, want: "INVALID_SERVICE_AUTH"},
	} {
		t.Run(test.want, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery("SELECT e.id,e.user_id").WithArgs(int64(1), int64(1)).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "integration", "enterprise", "kind", "status"}).AddRow(1, 1, "sep-test", "ent-test", "chat", "ready"))
			settings := &sepBalanceSettings{rate: "7.15", cfg: SEPConfig{Enabled: true, IntegrationID: "sep-test", SigningKeyID: "trusted", DirectBillingEnabled: test.enabled}}
			svc := &SEPService{db: db, settings: &SettingService{settingRepo: settings}}
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
			req.Header.Set("Authorization", "Bearer enterprise-key")
			req.Header.Set("X-SEP-Timestamp", fmt.Sprint(time.Now().Unix()))
			if test.mode != "" {
				req.Header.Set("X-Compute-Billing-Mode", test.mode)
			}
			if test.sepHeader != "" {
				req.Header.Set("X-SEP-Signature", test.sepHeader)
			}
			_, err = svc.AuthorizeGateway(t.Context(), req, &APIKey{ID: 1, UserID: 1, Key: "enterprise-key"})
			require.ErrorContains(t, err, test.want)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSEPPaymentCreditUsesImmutableOrderSnapshot(t *testing.T) {
	order := &dbent.PaymentOrder{ID: 1, Amount: 1, ProviderSnapshot: map[string]any{"platform_usd_to_cny_rate": "7.15", "sep_credit_cny": "7.150000"}}
	amount, rate, err := sepOrderCredit(order)
	require.NoError(t, err)
	require.Equal(t, "7.150000", amount)
	require.Equal(t, "7.15", rate)
	order.ProviderSnapshot = map[string]any{"platform_usd_to_cny_rate": "8"}
	amount, rate, err = sepOrderCredit(order)
	require.NoError(t, err)
	require.Equal(t, "8.000000", amount)
	require.Equal(t, "8", rate)
	order.ProviderSnapshot = nil
	_, _, err = sepOrderCredit(order)
	require.ErrorContains(t, err, "PAYMENT_RATE_SNAPSHOT_MISSING")
}
