package service

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

// Optional service-contract integration with the real SEP signed client and local database.
func TestSEPRotationSignedNodeIntegration(t *testing.T) {
	root := os.Getenv("SEP_ROTATION_NODE_ROOT")
	if root == "" || os.Getenv("COMPUTE_TEST_DATABASE_URL") == "" {
		t.Skip("set SEP_ROTATION_NODE_ROOT and the dedicated SEP test database")
	}
	svc, db, cfg, encryption := rotationTestService(t)
	enterprise := fmt.Sprintf("rotation-live-%d", time.Now().UnixNano())
	_, err := db.Exec(`UPDATE sep_enterprises SET enterprise_id=$1 WHERE id=1`, enterprise)
	require.NoError(t, err)
	public, signing, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	require.NoError(t, err)
	cfg.SigningKeyID = "sign-live"
	cfg.SigningPublicKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	directory := t.TempDir()
	writePrivate := func(name string, key any) string {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		path := filepath.Join(directory, name)
		require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
		return path
	}
	signingFile, encryptionFile := writePrivate("sign.pem", signing), writePrivate("enc.pem", encryption)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			_, err = svc.Authenticate(r.Context(), r, body)
		}
		var value any
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if err == nil && (len(parts) < 6 || parts[4] != enterprise || parts[5] != "credential-rotations" || r.Header.Get("X-SEP-Enterprise-ID") != enterprise) {
			err = sepMissing()
		}
		if err == nil {
			switch {
			case len(parts) == 6 && r.Method == "POST":
				var request SEPRotationRequest
				err = json.NewDecoder(bytes.NewReader(body)).Decode(&request)
				if err == nil && r.Header.Get("Idempotency-Key") != request.OperationID {
					err = sepConflict("IDEMPOTENCY_CONFLICT", "operation mismatch")
				}
				if err == nil {
					value, err = svc.RotateCredential(r.Context(), cfg, enterprise, request)
				}
			case len(parts) == 7 && r.Method == "GET":
				value, err = svc.Rotation(r.Context(), cfg.IntegrationID, enterprise, parts[6])
			case len(parts) == 8 && r.Method == "POST" && (parts[7] == "ack" || parts[7] == "retire"):
				value, err = svc.AdvanceRotation(r.Context(), cfg.IntegrationID, enterprise, parts[6], parts[7] == "retire")
			default:
				err = sepMissing()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(409)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "isolated contract request failed"}})
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	defer server.Close()
	cmd := exec.CommandContext(t.Context(), "node", "-r", "ts-node/register/transpile-only", "-r", "tsconfig-paths/register", "test/compute-credential-rotation.live.ts")
	cmd.Dir = filepath.Join(root, "backend")
	cmd.Env = append(os.Environ(), "SEP_ROTATION_CONTRACT_TEST=isolated-only", "ROTATION_TEST_ENTERPRISE_ID="+enterprise,
		"COMPUTE_API_ORIGIN="+server.URL, "COMPUTE_SIGNING_PRIVATE_KEY_FILE="+signingFile, "ROTATION_TEST_ENCRYPTION_FILE="+encryptionFile)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	var total, retired, ledgers int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*),COUNT(*) FILTER(WHERE generation_state='read_only') FROM sep_credentials`).Scan(&total, &retired))
	require.Equal(t, 6, total)
	require.Equal(t, 3, retired)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sep_ledger`).Scan(&ledgers))
	require.Zero(t, ledgers)
}

func rotationTestService(t *testing.T) (*SEPService, *sql.DB, *SEPConfig, *rsa.PrivateKey) {
	t.Helper()
	svc, db, settings := balanceTestService(t)
	_, err := db.Exec(`CREATE SEQUENCE fixture_keys START 100;
        ALTER TABLE api_keys ALTER COLUMN id SET DEFAULT nextval('fixture_keys');
        ALTER TABLE api_keys ADD COLUMN name TEXT,ADD COLUMN status TEXT DEFAULT 'active',ADD COLUMN deleted_at TIMESTAMPTZ,
        ADD COLUMN expires_at TIMESTAMPTZ,ADD COLUMN ip_whitelist JSONB,ADD COLUMN ip_blacklist JSONB,ADD COLUMN quota_used NUMERIC DEFAULT 0,
        ADD COLUMN usage_5h NUMERIC DEFAULT 0,ADD COLUMN usage_1d NUMERIC DEFAULT 0,ADD COLUMN usage_7d NUMERIC DEFAULT 0,
        ADD COLUMN window_5h_start TIMESTAMPTZ,ADD COLUMN window_1d_start TIMESTAMPTZ,ADD COLUMN window_7d_start TIMESTAMPTZ;
        BEGIN; SET LOCAL sep.key_management='on';
        INSERT INTO api_keys(id,user_id,key,group_id,name) VALUES(10,1,'old-chat',1,'chat'),(11,1,'old-image',2,'image'),(12,1,'old-video',3,'video');
        INSERT INTO sep_credentials(id,enterprise_pk,kind,api_key_id,encryption_key_id,masked_key,encrypted_key)
        VALUES('old-chat',1,'chat',10,'enc-test','****','fixture'),('old-image',1,'image',11,'enc-test','****','fixture'),('old-video',1,'video',12,'enc-test','****','fixture');
        COMMIT;`)
	require.NoError(t, err)
	private, err := rsa.GenerateKey(rand.Reader, 3072)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	require.NoError(t, err)
	settings.cfg.EncryptionKeyID = "enc-test"
	settings.cfg.EncryptionPublicKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	return svc, db, &settings.cfg, private
}

func rotationRequest(id, kind string) SEPRotationRequest {
	return SEPRotationRequest{ContractVersion: 1, OperationID: id, Kind: kind, ExpectedCredentialID: "old-" + kind, ExpectedVersion: 1, EncryptionKeyID: "enc-test"}
}

func TestSEPRotationConcurrentReplayAndReadOnlyRetirement(t *testing.T) {
	svc, db, cfg, private := rotationTestService(t)
	_, policyErr := db.Exec(`BEGIN; SET LOCAL sep.key_management='on'; UPDATE api_keys SET quota=100,quota_used=30,rate_limit_1d=25,ip_whitelist='["127.0.0.1"]',expires_at='2030-01-01' WHERE id=10; COMMIT;`)
	require.NoError(t, policyErr)
	request := rotationRequest("rotate-once", "chat")
	var wg sync.WaitGroup
	receipts := make(chan *SEPRotationResult, 8)
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := svc.RotateCredential(t.Context(), cfg, "ent-test", request)
			receipts <- v
			failures <- err
		}()
	}
	wg.Wait()
	close(receipts)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	var first *SEPRotationResult
	for v := range receipts {
		if first == nil {
			first = v
		}
		require.Equal(t, first, v)
	}
	require.Equal(t, "prepared", first.Status)
	envelope, err := jose.ParseEncrypted(first.Credential.EncryptedKey, []jose.KeyAlgorithm{jose.RSA_OAEP_256}, []jose.ContentEncryption{jose.A256GCM})
	require.NoError(t, err)
	clear, err := envelope.Decrypt(private)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(clear, &payload))
	require.Equal(t, "ent-test", payload["enterprise_id"])
	require.Equal(t, float64(2), payload["credential_version"])
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_credentials WHERE kind='chat'`).Scan(&count))
	require.Equal(t, 2, count)
	var current string
	require.NoError(t, db.QueryRow(`SELECT id FROM sep_credentials WHERE kind='chat' AND is_current`).Scan(&current))
	require.Equal(t, "old-chat", current)
	var policyPreserved bool
	require.NoError(t, db.QueryRow(`SELECT k.quota=100 AND k.quota_used=30 AND k.rate_limit_1d=25 AND k.ip_whitelist='["127.0.0.1"]'::jsonb AND k.expires_at='2030-01-01'::timestamptz
        FROM api_keys k JOIN sep_credentials c ON c.api_key_id=k.id WHERE c.id=$1`, first.Credential.CredentialID).Scan(&policyPreserved))
	require.True(t, policyPreserved)
	changed := request
	changed.Kind = "image"
	_, err = svc.RotateCredential(t.Context(), cfg, "ent-test", changed)
	require.ErrorContains(t, err, "IDEMPOTENCY_CONFLICT")
	changed = rotationRequest("second-operation", "chat")
	_, err = svc.RotateCredential(t.Context(), cfg, "ent-test", changed)
	require.ErrorContains(t, err, "CREDENTIAL_ROTATION_IN_PROGRESS")
	_, err = svc.AdvanceRotation(t.Context(), cfg.IntegrationID, "ent-test", request.OperationID, true)
	require.ErrorContains(t, err, "CREDENTIAL_NOT_ACKNOWLEDGED")
	for i := 0; i < 2; i++ {
		v, err := svc.AdvanceRotation(t.Context(), cfg.IntegrationID, "ent-test", request.OperationID, false)
		require.NoError(t, err)
		require.Equal(t, "acknowledged", v.Status)
	}
	require.NoError(t, db.QueryRow(`SELECT id FROM sep_credentials WHERE kind='chat' AND is_current`).Scan(&current))
	require.Equal(t, first.Credential.CredentialID, current)
	for i := 0; i < 2; i++ {
		v, err := svc.AdvanceRotation(t.Context(), cfg.IntegrationID, "ent-test", request.OperationID, true)
		require.NoError(t, err)
		require.Equal(t, "completed", v.Status)
	}
	old := &APIKey{ID: 10, SEP: &SEPExecution{EnterprisePK: 1}}
	require.ErrorContains(t, checkSEPRotatedCredential(t.Context(), db, old, "/v1/chat/completions", true, false), "CREDENTIAL_RETIRED")
	require.ErrorContains(t, checkSEPRotatedCredential(t.Context(), db, old, "/v1/models", false, false), "CREDENTIAL_RETIRED")
	_, err = svc.Rotation(t.Context(), cfg.IntegrationID, "other-enterprise", request.OperationID)
	require.ErrorContains(t, err, "SEP_RESOURCE_NOT_FOUND")
	var balance string
	require.NoError(t, db.QueryRow(`SELECT available_cny::text FROM sep_enterprises WHERE id=1`).Scan(&balance))
	require.Equal(t, "0.000000", balance)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sep_ledger`).Scan(&count))
	require.Zero(t, count)
}

func TestSEPRotationConcurrentDifferentOperationsAndTaskProtection(t *testing.T) {
	svc, db, cfg, _ := rotationTestService(t)
	var wg sync.WaitGroup
	failures := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.RotateCredential(t.Context(), cfg, "ent-test", rotationRequest(fmt.Sprintf("op-%d", i), "video"))
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
			require.ErrorContains(t, err, "CREDENTIAL_ROTATION_IN_PROGRESS")
		}
	}
	require.Equal(t, 1, success)
	var operation string
	require.NoError(t, db.QueryRow(`SELECT operation_id FROM sep_credential_rotations WHERE kind='video'`).Scan(&operation))
	_, err := svc.AdvanceRotation(t.Context(), cfg.IntegrationID, "ent-test", operation, false)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sep_executions(id,integration_id,enterprise_pk,execution_id,api_key_id,kind,model,request_hash,exchange_rate,price_snapshot,multiplier_snapshot,task_id)
        VALUES('video-task','sep-test',1,'video-task',12,'video','test','hash',7.15,'{}','{}','old-task')`)
	require.NoError(t, err)
	_, err = svc.AdvanceRotation(t.Context(), cfg.IntegrationID, "ent-test", operation, true)
	require.ErrorContains(t, err, "CREDENTIAL_TASKS_PENDING")
	_, err = db.Exec(`UPDATE sep_executions SET execution_status='completed',fee_status='final',gross_cny=0,bill_id='bill-video',bill_version=1 WHERE id='video-task';
        INSERT INTO sep_settlements(integration_id,settlement_id,execution_pk,request_hash,gross_cny,free_cny,paid_cny,receipt)VALUES('sep-test','settled-video','video-task','hash',0,0,0,'{}')`)
	require.NoError(t, err)
	_, err = svc.AdvanceRotation(t.Context(), cfg.IntegrationID, "ent-test", operation, true)
	require.NoError(t, err)
	old := &APIKey{ID: 12, SEP: &SEPExecution{EnterprisePK: 1}}
	require.NoError(t, checkSEPRotatedCredential(t.Context(), db, old, "/v1/videos/old-task/content", false, false))
	require.ErrorContains(t, checkSEPRotatedCredential(t.Context(), db, old, "/v1/videos/foreign-task", false, false), "SEP_RESOURCE_NOT_FOUND")
	_, err = svc.RotateCredential(t.Context(), cfg, "ent-test", rotationRequest("image-parallel", "image"))
	require.NoError(t, err)
}
