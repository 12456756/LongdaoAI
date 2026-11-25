package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVideoSubmissionRetryRecoveryUsesIdenticalRequestAndBillsOnce(t *testing.T) {
	s, key, account, ledger := newVideoContractFixture(t)
	var delays []time.Duration
	s.videoSubmissionRetryWait = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	var bodies [][]byte
	var keys []string
	posts := 0
	s.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" {
			return legacyTestResponse(200, "seedance-2.0", "succeeded", "https://cdn.example/video.mp4"), nil
		}
		posts++
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		bodies = append(bodies, body)
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		require.Equal(t, "/v1/video/tasks", r.URL.Path)
		require.Equal(t, "Bearer secret-upstream", r.Header.Get("Authorization"))
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.InDelta(t, 60, time.Until(deadline).Seconds(), 1)
		switch posts {
		case 1:
			return nil, io.ErrUnexpectedEOF
		case 2:
			return asyncResponse(503, `{"message":"capacity temporarily unavailable","reason":"capacity_busy"}`), nil
		case 3:
			return asyncResponse(202, `{"code":0,"message":"accepted","data":{}}`), nil
		default:
			return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
		}
	}}
	status, out, err := createVideoContract(t, s, key, account, "retry-original")
	require.NoError(t, err)
	require.Equal(t, 202, status)
	require.Equal(t, "accepted", out["submission_status"])
	require.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, delays)
	retry := out["submission_retry"].(map[string]any)
	require.EqualValues(t, 4, retry["attempts"])
	require.Equal(t, false, retry["exhausted"])
	require.Len(t, retry["errors"], 3)
	require.NotEmpty(t, keys[0])
	for i := range keys {
		require.Equal(t, keys[0], keys[i])
		require.Equal(t, bodies[0], bodies[i])
	}
	require.Equal(t, "pending", ledger.rows["client:retry-original"].Status)
	require.Empty(t, ledger.commands)
	for i := 0; i < 2; i++ {
		c, w := videoTestContext("GET", "/v1/videos/"+legacyTestTaskID, nil, "")
		_, err = s.ForwardVideo(context.Background(), c, account, key, nil, legacyTestTaskID, false, "")
		require.NoError(t, err)
		require.Contains(t, w.Body.String(), `"status":"settled"`)
		require.Contains(t, w.Body.String(), `"attempts":4`)
	}
	require.Len(t, ledger.commands, 1)
	require.Equal(t, 4, posts)
}

func TestVideoSubmissionRetryExhaustedClosesBillingAndCannotResubmit(t *testing.T) {
	s, key, account, ledger := newVideoContractFixture(t)
	calls := 0
	s.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) {
		calls++
		return nil, io.ErrUnexpectedEOF
	}}
	status, out, err := createVideoContract(t, s, key, account, "retry-exhausted")
	require.ErrorContains(t, err, "unexpected EOF")
	require.Equal(t, 502, status)
	require.Equal(t, "failed", out["submission_status"])
	require.Nil(t, out["status"])
	require.Nil(t, out["task_id"])
	retry := out["submission_retry"].(map[string]any)
	require.EqualValues(t, 4, retry["attempts"])
	require.Equal(t, true, retry["exhausted"])
	require.Len(t, retry["errors"], 4)
	require.Equal(t, "not_charged", out["billing"].(map[string]any)["status"])
	require.Equal(t, "0", out["billing"].(map[string]any)["actual_cost_usd"])
	_, replay, err := createVideoContract(t, s, key, account, "retry-exhausted")
	require.Error(t, err)
	require.Equal(t, true, replay["submission_retry"].(map[string]any)["exhausted"])
	c, w := videoTestContext("GET", "/v1/videos/retry-exhausted?id_type=request_id", nil, "")
	_, done, err := s.ResolveVideoQuery(context.Background(), c, key, "retry-exhausted")
	require.NoError(t, err)
	require.True(t, done)
	require.Contains(t, w.Body.String(), `"submission_status":"failed"`)
	require.Contains(t, w.Body.String(), `"status":"not_charged"`)
	require.Equal(t, 4, calls)
	require.Empty(t, ledger.commands)
}

func TestVideoSubmissionRetrySurvivesCallerDisconnect(t *testing.T) {
	s, _, account, _ := newVideoContractFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := &videoSubmission{Retry: &VideoSubmissionRetry{MaxAttempts: 4}}
	calls := 0
	s.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		calls++
		require.NoError(t, r.Context().Err())
		if calls == 1 {
			cancel()
			return nil, io.ErrUnexpectedEOF
		}
		return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
	}}
	task, err := s.createLegacyVideoWithRetry(ctx, account, []byte(`{}`), "same-idem", sub, func() error { return nil })
	require.NoError(t, err)
	require.Equal(t, legacyTestTaskID, task.ID)
	require.Equal(t, 2, calls)
	require.Equal(t, 2, sub.Retry.Attempts)
}

func TestVideoSubmissionRetryReusesReferenceURLsWithoutUploadingAgain(t *testing.T) {
	s, key, account, _ := newVideoContractFixture(t)
	store := s.cache.(*videoMemoryStore)
	var original []byte
	calls := 0
	s.httpUpstream = videoTestUpstream{do: func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Contains(t, string(body), "reference_image")
		if calls == 1 {
			original = body
		} else {
			require.Equal(t, original, body)
		}
		require.Len(t, store.references, 1)
		if calls < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		return legacyTestResponse(202, "seedance-2.0", "queued", ""), nil
	}}
	body, ct := videoMultipartFixture(t, legacyTestPNG(t))
	parsed, err := ParseOpenAIVideoRequest(body, ct)
	require.NoError(t, err)
	c, w := videoTestContext("POST", "/v1/videos", body, ct)
	c.Request.Header.Set("X-Client-Request-ID", "retry-reference")
	c.Request.Header.Set("Idempotency-Key", "retry-reference")
	c.Request.Header.Set("X-Video-Operation", "image-to-video")
	require.NoError(t, InitVideoContract(c))
	_, err = s.ForwardVideo(c.Request.Context(), c, account, key, parsed, "", false, "")
	require.NoError(t, err)
	require.Equal(t, 202, w.Code)
	require.Len(t, store.references, 1)
	require.Equal(t, 4, calls)
}

func TestVideoSubmissionInterruptedRecordsCloseWithoutDispatch(t *testing.T) {
	for _, state := range []string{"unknown", "dispatching", "legacy_dispatching"} {
		t.Run(state, func(t *testing.T) {
			s, key, _, ledger := newVideoContractFixture(t)
			id := "interrupted-" + state
			sub := &videoSubmission{SchemaVersion: 1, Protocol: VideoProtocolLegacy, State: state, RequestID: id, APIKeyID: key.ID, GroupID: *key.GroupID, ExpiresAt: time.Now().Add(time.Hour).Unix(), DispatchDeadline: time.Now().Add(-time.Minute).Unix(), BillingRegistered: true, BillingRequestID: "client:" + id, Retry: &VideoSubmissionRetry{Attempts: 2, MaxAttempts: 4}}
			if state == "legacy_dispatching" {
				sub.State = "dispatching"
				sub.DispatchDeadline = 0
			}
			ledger.rows[sub.BillingRequestID] = &VideoBillingOutcome{APIKeyID: key.ID, UserID: key.UserID, Status: "pending"}
			store, _ := s.videoStore()
			storageKey := videoSubmissionKey(key, id)
			_, err := store.VideoSave(context.Background(), storageKey, mustVideoDiagnosticJSON(sub), time.Unix(sub.ExpiresAt, 0), true)
			require.NoError(t, err)
			_, err = store.VideoSave(context.Background(), videoRequestIndexKey(key, id), []byte(storageKey), time.Unix(sub.ExpiresAt, 0), true)
			require.NoError(t, err)
			c, w := videoTestContext("GET", "/v1/videos/"+id+"?id_type=request_id", nil, "")
			_, done, err := s.ResolveVideoQuery(context.Background(), c, key, id)
			require.NoError(t, err)
			require.True(t, done)
			require.Contains(t, w.Body.String(), `"submission_status":"failed"`)
			require.Contains(t, w.Body.String(), "video_submission_interrupted")
			require.Equal(t, "not_charged", ledger.rows[sub.BillingRequestID].Status)
			persisted, err := s.loadVideoSubmission(context.Background(), key, id)
			require.NoError(t, err)
			require.Equal(t, "failed", persisted.State)
		})
	}
}

func TestVideoSubmissionBillingFailureIsPlatformErrorAndCanCloseLater(t *testing.T) {
	s, key, account, ledger := newVideoContractFixture(t)
	ledger.failClose = true
	s.httpUpstream = videoTestUpstream{do: func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }}
	_, out, err := createVideoContract(t, s, key, account, "billing-close-failed")
	require.Error(t, err)
	require.Equal(t, "failed", out["submission_status"])
	billing := out["billing"].(map[string]any)
	require.Equal(t, "unknown", billing["status"])
	require.Equal(t, "platform", billing["error"].(map[string]any)["source"])
	require.Contains(t, billing["error"].(map[string]any)["message"], "billing close transaction unavailable")
	ledger.failClose = false
	c, w := videoTestContext("GET", "/v1/videos/billing-close-failed?id_type=request_id", nil, "")
	_, done, err := s.ResolveVideoQuery(context.Background(), c, key, "billing-close-failed")
	require.NoError(t, err)
	require.True(t, done)
	var output map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &output))
	require.Equal(t, "not_charged", output["billing"].(map[string]any)["status"])
	require.Empty(t, ledger.commands)
}
