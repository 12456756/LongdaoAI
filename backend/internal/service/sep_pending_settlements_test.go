package service

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func pendingTestDecision(t *testing.T, db *sql.DB, id, paid, free string) SEPSettlement {
	t.Helper()
	_, err := db.Exec(`INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,fee_status,gross_cny,bill_id,bill_version)
        VALUES($1,'sep-test',1,$1,1,'chat','chat','hash',7.15,'{}','{}','final',$2::numeric+$3::numeric,$4,1)`, id, paid, free, "bill-"+id)
	require.NoError(t, err)
	var gross string
	require.NoError(t, db.QueryRow(`SELECT gross_cny::text FROM sep_executions WHERE id=$1`, id).Scan(&gross))
	return SEPSettlement{ContractVersion: 1, EnterpriseID: "ent-test", SettlementID: "settle-" + id, BillID: "bill-" + id,
		BillVersion: 1, Currency: "CNY", GrossCNY: gross, FreeCNY: free, PaidCNY: paid}
}

func TestSEPPendingWholeBillRecoveryAndImmutableIdentity(t *testing.T) {
	svc, db, _ := balanceTestService(t)
	_, err := svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "fund", Operation: "add", Amount: "20", Currency: "CNY"})
	require.NoError(t, err)
	first := pendingTestDecision(t, db, "pending-30", "30.000000", "2.000000")
	for i := 0; i < 3; i++ {
		_, err = svc.Settle(t.Context(), "sep-test", "pending-30", first)
		require.ErrorContains(t, err, "INSUFFICIENT_PAID_FUNDS")
	}
	var balance string
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "20.000000", balance, "pending bill never pretends to debit part of the money")
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_pending_settlements`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_settlements`).Scan(&count))
	require.Zero(t, count)
	changed := first
	changed.SettlementID = "another-id"
	_, err = svc.Settle(t.Context(), "sep-test", "pending-30", changed)
	require.ErrorContains(t, err, "IDEMPOTENCY_CONFLICT")
	second := pendingTestDecision(t, db, "pending-1", "1.000000", "0.000000")
	collision := second
	collision.SettlementID = first.SettlementID
	_, err = svc.Settle(t.Context(), "sep-test", "pending-1", collision)
	require.ErrorContains(t, err, "IDEMPOTENCY_CONFLICT")
	_, err = svc.Settle(t.Context(), "sep-test", "pending-1", second)
	require.ErrorContains(t, err, "INSUFFICIENT_PAID_FUNDS", "later small bill cannot bypass earlier debt")
	free := pendingTestDecision(t, db, "free", "0.000000", "1.000000")
	_, err = svc.Settle(t.Context(), "sep-test", "free", free)
	require.NoError(t, err, "existing free work is settled without cash")
	_, err = svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "recharge", Operation: "add", Amount: "10", Currency: "CNY"})
	require.NoError(t, err)
	require.NoError(t, svc.retryPendingSettlements(t.Context()))
	receipt, err := svc.Settle(t.Context(), "sep-test", "pending-30", first)
	require.NoError(t, err)
	again, err := svc.Settle(t.Context(), "sep-test", "pending-30", first)
	require.NoError(t, err)
	require.Equal(t, receipt, again)
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "0.000000", balance)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_pending_settlements`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "recharge-next", Operation: "add", Amount: "1", Currency: "CNY"})
	require.NoError(t, err)
	require.NoError(t, svc.retryPendingSettlements(t.Context()))
	require.NoError(t, svc.retryPendingSettlements(t.Context()))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_pending_settlements`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_ledger WHERE type='paid_consumption'`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestSEPPendingConcurrentBillsCannotOverdrawAndRecoverOnce(t *testing.T) {
	svc, db, _ := balanceTestService(t)
	_, err := svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "fund", Operation: "add", Amount: "5", Currency: "CNY"})
	require.NoError(t, err)
	decisions := make([]SEPSettlement, 12)
	for i := range decisions {
		decisions[i] = pendingTestDecision(t, db, fmt.Sprintf("run-%02d", i), "1.000000", "0.000000")
	}
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := range decisions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Settle(t.Context(), "sep-test", fmt.Sprintf("run-%02d", i), decisions[i])
			failures <- err
		}(i)
	}
	wg.Wait()
	close(failures)
	success := 0
	for err := range failures {
		if err == nil {
			success++
		} else {
			require.ErrorContains(t, err, "INSUFFICIENT_PAID_FUNDS")
		}
	}
	require.Equal(t, 5, success)
	_, err = svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "topup", Operation: "add", Amount: "7", Currency: "CNY"})
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		require.NoError(t, svc.retryPendingSettlements(t.Context()))
	}
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_pending_settlements`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_settlements`).Scan(&count))
	require.Equal(t, 12, count)
	var balance string
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "0.000000", balance)
}

func TestSEPPendingSettlementAuditFailureRollsBackOriginalBill(t *testing.T) {
	svc, db, _ := balanceTestService(t)
	decision := pendingTestDecision(t, db, "rollback", "2.000000", "0.000000")
	_, err := svc.Settle(t.Context(), "sep-test", "rollback", decision)
	require.ErrorContains(t, err, "INSUFFICIENT_PAID_FUNDS")
	_, err = svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "fund", Operation: "add", Amount: "2", Currency: "CNY"})
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE sep_balance_events ADD CONSTRAINT block_settlement CHECK(false) NOT VALID`)
	require.NoError(t, err)
	require.Error(t, svc.retryPendingSettlements(t.Context()))
	var balance string
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "2.000000", balance)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_pending_settlements`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = db.Exec(`ALTER TABLE sep_balance_events DROP CONSTRAINT block_settlement`)
	require.NoError(t, err)
	require.NoError(t, svc.retryPendingSettlements(t.Context()))
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "0.000000", balance)
}

type pendingGatePrices struct{ ModelBasePriceRepository }

func TestSEPPendingDebtBlocksNewGenerationButAllowsBillQueries(t *testing.T) {
	svc, db, settings := balanceTestService(t)
	decision := pendingTestDecision(t, db, "debt", "30.000000", "0.000000")
	_, err := svc.Settle(t.Context(), "sep-test", "debt", decision)
	require.ErrorContains(t, err, "INSUFFICIENT_PAID_FUNDS")
	_, err = svc.AdjustAccountBalance(t.Context(), SEPBalanceChange{UserID: 1, BusinessRef: "fund", Operation: "add", Amount: "20", Currency: "CNY"})
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sep_credentials(id,enterprise_pk,kind,api_key_id,encryption_key_id,masked_key,encrypted_key) VALUES('chat-credential',1,'chat',1,'fixture','****','fixture')`)
	require.NoError(t, err)
	settings.cfg.DirectBillingEnabled = true
	svc.billing = &BillingService{basePrices: &pendingGatePrices{}}
	group := int64(1)
	key := &APIKey{ID: 1, UserID: 1, Key: "fixture-enterprise-key", GroupID: &group, Group: &Group{ID: group}}
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture"}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("X-Compute-Billing-Mode", "direct")
	request.Header.Set("Idempotency-Key", "new-work")
	_, err = svc.AuthorizeGateway(t.Context(), request, key)
	require.ErrorContains(t, err, "ENTERPRISE_BILL_OUTSTANDING")
	for _, path := range []string{"/v1/models", "/v1/sub2api/billing"} {
		read := httptest.NewRequest("GET", path, nil)
		read.Header.Set("Authorization", "Bearer "+key.Key)
		read.Header.Set("X-Compute-Billing-Mode", "direct")
		_, err = svc.AuthorizeGateway(t.Context(), read, key)
		require.NoError(t, err, "debt must not prevent diagnosis or original bill queries")
	}
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_executions WHERE execution_id='new-work'`).Scan(&count))
	require.Zero(t, count)
}
