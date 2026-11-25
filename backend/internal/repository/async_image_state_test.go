package repository

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAsyncImageClaimConcurrentAliasesTerminalCASAndTTL(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := NewImageTaskStore(client).(service.AsyncImageTaskStore)
	ctx := context.Background()
	var created atomic.Int32
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 16)
	ids := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			record := &service.ImageTaskRecord{ID: fmt.Sprintf("imgtask_%d", index), APIKeyID: 7, UserID: 11, Status: "processing", Provider: &service.AsyncImageMetadata{RequestID: "r1", IdempotencyKey: "i1", SubmissionStatus: "submitting"}}
			result, won, err := store.Claim(ctx, record, 24*time.Hour)
			if err != nil {
				errorsSeen <- err
				return
			}
			if won {
				created.Add(1)
			}
			ids <- result.ID
		}(i)
	}
	wg.Wait()
	close(errorsSeen)
	close(ids)
	for err := range errorsSeen {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, created.Load())
	var id string
	for current := range ids {
		if id == "" {
			id = current
		}
		require.Equal(t, id, current)
	}
	stored, err := store.FindClaim(ctx, 7, "r1", "i1")
	require.NoError(t, err)
	require.Equal(t, id, stored.ID)
	other, err := store.FindClaim(ctx, 8, "r1", "i1")
	require.NoError(t, err)
	require.Nil(t, other)
	stored.Provider.SubmissionStatus = "accepted"
	stored.Provider.UpstreamTaskID = "imgjob_fixture"
	require.NoError(t, store.Save(ctx, stored, 24*time.Hour))
	stale := *stored
	stored.Status = "completed"
	stored.Provider.ResultURL = "https://images.example/a.png"
	require.NoError(t, store.Save(ctx, stored, 24*time.Hour))
	stale.Status = "failed"
	require.NoError(t, store.Save(ctx, &stale, 24*time.Hour))
	got, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "completed", got.Status)
	server.FastForward(24 * time.Hour)
	_, err = store.Get(ctx, id)
	require.ErrorIs(t, err, service.ErrImageTaskNotFound)
	other, err = store.FindClaim(ctx, 7, "r1", "i1")
	require.NoError(t, err)
	require.Nil(t, other)
}

func TestAsyncImageRequestLookupDoesNotMatchIdempotencyAliases(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewImageTaskStore(client).(service.AsyncImageTaskStore)
	ctx := context.Background()
	for _, record := range []*service.ImageTaskRecord{
		{ID: "imgtask_first", APIKeyID: 7, UserID: 11, Status: "processing", Provider: &service.AsyncImageMetadata{RequestID: "request-one", IdempotencyKey: "request-two", SubmissionStatus: "submitting"}},
		{ID: "imgtask_second", APIKeyID: 7, UserID: 11, Status: "processing", Provider: &service.AsyncImageMetadata{RequestID: "request-two", IdempotencyKey: "idem-two", SubmissionStatus: "submitting"}},
	} {
		_, created, err := store.Claim(ctx, record, 24*time.Hour)
		require.NoError(t, err)
		require.True(t, created)
	}
	got, err := store.FindClaim(ctx, 7, "request-two", "")
	require.NoError(t, err)
	require.Equal(t, "imgtask_second", got.ID)
	got, err = store.FindClaim(ctx, 7, "idem-two", "")
	require.NoError(t, err)
	require.Nil(t, got)
	got, err = store.FindClaim(ctx, 7, "", "request-two")
	require.NoError(t, err)
	require.Equal(t, "imgtask_first", got.ID)
	_, err = store.FindClaim(ctx, 7, "request-two", "request-two")
	require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
}
