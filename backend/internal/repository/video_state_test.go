package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestVideoStateFixedExpiryAndTerminalStatus(t *testing.T) {
	r := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: r.Addr()})
	t.Cleanup(func() { client.Close() })
	store := &gatewayCache{rdb: client}
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	m := &service.VideoTaskMetadata{SchemaVersion: 1, TaskID: "mtask_example", APIKeyID: 4, GroupID: 3, AccountID: 8, Protocol: service.VideoProtocolLegacy, LastStatus: "queued", BoundAt: now.Unix(), ExpiresAt: now.Add(7 * 24 * time.Hour).Unix()}
	require.NoError(t, store.VideoBind(ctx, m))
	key := service.VideoTaskMetadataKey(3, 4, m.TaskID)
	binding, e := client.Get(ctx, buildSessionKey(3, "openai-video:4:"+service.DeriveSessionHashFromSeed(m.TaskID))).Int64()
	require.NoError(t, e)
	require.Equal(t, int64(8), binding)
	r.FastForward(time.Hour)
	ttl := r.TTL(key)
	status, e := store.VideoStatus(ctx, key, "completed")
	require.NoError(t, e)
	require.Equal(t, "completed", status)
	status, e = store.VideoStatus(ctx, key, "in_progress")
	require.NoError(t, e)
	require.Equal(t, "completed", status)
	require.Equal(t, ttl, r.TTL(key))
	require.NoError(t, store.VideoBind(ctx, m))
	b, e := store.VideoLoad(ctx, key)
	require.NoError(t, e)
	var saved service.VideoTaskMetadata
	require.NoError(t, json.Unmarshal(b, &saved))
	require.Equal(t, "completed", saved.LastStatus)
	require.Equal(t, ttl, r.TTL(key))
	ref := &service.VideoReference{SchemaVersion: 1, AssetID: "vref_test", ExpiresAt: m.ExpiresAt, SignedURL: "https://relay.example/signed", ByteLength: 12}
	require.NoError(t, store.VideoSaveReference(ctx, ref))
	require.NoError(t, store.VideoReferenceTask(ctx, ref.AssetID, m.TaskID))
	got, e := store.VideoLoadReference(ctx, ref.AssetID)
	require.NoError(t, e)
	require.Equal(t, ref.SignedURL, got.SignedURL)
	require.Equal(t, m.TaskID, got.TaskID)
	r.FastForward(8 * 24 * time.Hour)
	require.NoError(t, store.VideoReferenceTask(ctx, ref.AssetID, "late"))
	require.False(t, r.Exists("video:reference:"+ref.AssetID))
	ok, e := store.VideoReplace(ctx, key, []byte(`{}`))
	require.NoError(t, e)
	require.False(t, ok)
}
