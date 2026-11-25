package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var _ service.VideoStateStore = (*gatewayCache)(nil)

func (c *gatewayCache) VideoLoad(ctx context.Context, key string) ([]byte, error) {
	return c.rdb.Get(ctx, key).Bytes()
}

func (c *gatewayCache) VideoSave(ctx context.Context, key string, data []byte, expires time.Time, nx bool) (bool, error) {
	args := &redis.SetArgs{ExpireAt: expires}
	if nx {
		args.Mode = "NX"
	}
	result, err := c.rdb.SetArgs(ctx, key, data, *args).Result()
	if err == redis.Nil {
		return false, nil
	}
	return err == nil && result == "OK", err
}

func (c *gatewayCache) VideoReplace(ctx context.Context, key string, data []byte) (bool, error) {
	result, err := c.rdb.SetArgs(ctx, key, data, redis.SetArgs{Mode: "XX", KeepTTL: true}).Result()
	if err == redis.Nil {
		return false, nil
	}
	return result == "OK", err
}

func (c *gatewayCache) VideoDeleteIfEqual(ctx context.Context, key string, data []byte) error {
	return c.rdb.Eval(ctx, `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, []string{key}, data).Err()
}

func (c *gatewayCache) VideoReplaceIfEqual(ctx context.Context, key string, previous, data []byte) (bool, error) {
	updated, err := c.rdb.Eval(ctx, `if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
redis.call('SET',KEYS[1],ARGV[2],'XX','KEEPTTL'); return 1`, []string{key}, previous, data).Int()
	return updated == 1, err
}

func (c *gatewayCache) VideoBind(ctx context.Context, m *service.VideoTaskMetadata) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	expires := time.Unix(m.ExpiresAt, 0)
	commands, err := c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		session := fmt.Sprintf("openai-video:%d:%s", m.APIKeyID, service.DeriveSessionHashFromSeed(m.TaskID))
		p.SetArgs(ctx, buildSessionKey(m.GroupID, session), m.AccountID, redis.SetArgs{ExpireAt: expires, Mode: "NX"})
		p.SetArgs(ctx, service.VideoTaskMetadataKey(m.GroupID, m.APIKeyID, m.TaskID), data, redis.SetArgs{ExpireAt: expires, Mode: "NX"})
		return nil
	})
	if err != nil && err != redis.Nil {
		return err
	}
	for _, command := range commands {
		if e := command.Err(); e != nil && e != redis.Nil {
			return e
		}
	}
	return nil
}

func (c *gatewayCache) VideoStatus(ctx context.Context, key, status string) (string, error) {
	return c.rdb.Eval(ctx, `local v=redis.call('GET',KEYS[1]); if not v then return redis.error_reply('missing video metadata') end
 local m=cjson.decode(v); local old=m.last_status
 if old=='completed' or old=='failed' or old=='cancelled' then return old end
 m.last_status=ARGV[1]; redis.call('SET',KEYS[1],cjson.encode(m),'XX','KEEPTTL'); return ARGV[1]`, []string{key}, status).Text()
}

func (c *gatewayCache) VideoObserve(ctx context.Context, key, status string, observed int64, failure map[string]any) (*service.VideoTaskMetadata, error) {
	detail, err := json.Marshal(failure)
	if err != nil {
		return nil, err
	}
	data, err := c.rdb.Eval(ctx, `local v=redis.call('GET',KEYS[1]); if not v then return redis.error_reply('missing video metadata') end
local m=cjson.decode(v); local old=m.last_status
if old=='completed' or old=='failed' or old=='cancelled' then
 if old==ARGV[1] and tonumber(m.observed_at or 0)<=tonumber(ARGV[2]) then
 m.observed_at=tonumber(ARGV[2]); if ARGV[3]~='null' then m.task_error=cjson.decode(ARGV[3]) end
 local updated=cjson.encode(m); redis.call('SET',KEYS[1],updated,'XX','KEEPTTL'); return updated end
 return v end
if tonumber(m.observed_at or 0)>tonumber(ARGV[2]) then return v end
if old=='in_progress' and ARGV[1]=='queued' then return v end
m.last_status=ARGV[1]; m.observed_at=tonumber(ARGV[2]); m.task_error=cjson.decode(ARGV[3])
	local updated=cjson.encode(m); redis.call('SET',KEYS[1],updated,'XX','KEEPTTL'); return updated`, []string{key}, status, observed, string(detail)).Text()
	if err != nil {
		return nil, err
	}
	var meta service.VideoTaskMetadata
	if err = json.Unmarshal([]byte(data), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (c *gatewayCache) VideoSaveReference(ctx context.Context, r *service.VideoReference) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return err
	}
	values := make(map[string]any, len(fields))
	for k, v := range fields {
		values[k] = string(v)
	}
	_, err = c.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		key := "video:reference:" + r.AssetID
		p.HSet(ctx, key, values)
		expires := r.ExpiresAt
		if r.ReadUntil > 0 {
			expires = r.ReadUntil + 86400
		}
		p.ExpireAt(ctx, key, time.Unix(expires, 0))
		return nil
	})
	return err
}

func (c *gatewayCache) VideoLoadReference(ctx context.Context, asset string) (*service.VideoReference, error) {
	fields, err := c.rdb.HGetAll(ctx, "video:reference:"+asset).Result()
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, redis.Nil
	}
	values := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		values[k] = json.RawMessage(v)
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	var r service.VideoReference
	err = json.Unmarshal(data, &r)
	return &r, err
}

func (c *gatewayCache) VideoReferenceTask(ctx context.Context, asset, task string) error {
	encoded, _ := json.Marshal(task)
	return c.rdb.Eval(ctx, `if redis.call('EXISTS',KEYS[1])==0 then return 0 end
 redis.call('HSET',KEYS[1],'task_id',ARGV[1]); return 1`, []string{"video:reference:" + asset}, string(encoded)).Err()
}

func (c *gatewayCache) VideoDeleteReference(ctx context.Context, asset string) error {
	return c.rdb.Del(ctx, "video:reference:"+asset).Err()
}
