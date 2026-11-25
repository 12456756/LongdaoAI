package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const imageTaskKeyPrefix = "image_task:"

type imageTaskStore struct {
	rdb *redis.Client
}

func NewImageTaskStore(rdb *redis.Client) service.ImageTaskStore {
	return &imageTaskStore{rdb: rdb}
}

func (s *imageTaskStore) Save(ctx context.Context, task *service.ImageTaskRecord, ttl time.Duration) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	if task.Provider != nil {
		return s.rdb.Eval(ctx, `local old=redis.call('GET',KEYS[1]); if not old then return redis.error_reply('image task expired') end
local a=cjson.decode(old); local b=cjson.decode(ARGV[1]);
if a.provider and (a.status=='completed' or a.status=='failed' or (a.provider.submission_status=='accepted' and b.provider.submission_status~='accepted')) then return 0 end
redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2]); return 1`, []string{imageTaskKey(task.ID)}, string(data), ttl.Milliseconds()).Err()
	}
	return s.rdb.Set(ctx, imageTaskKey(task.ID), data, ttl).Err()
}

func asyncImageAlias(keyID int64, kind, id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("image_task_alias:%d:%s:%x", keyID, kind, sum)
}

func (s *imageTaskStore) FindClaim(ctx context.Context, keyID int64, requestID, idem string) (*service.ImageTaskRecord, error) {
	var keys []string
	if requestID != "" {
		keys = append(keys, asyncImageAlias(keyID, "request", requestID))
	}
	if idem != "" {
		keys = append(keys, asyncImageAlias(keyID, "idempotency", idem))
	}
	if len(keys) == 0 {
		return nil, nil
	}
	aliases, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	var id string
	for _, value := range aliases {
		if value == nil {
			continue
		}
		current, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("Invalid image submission alias")
		}
		if id != "" && id != current {
			return nil, service.ErrUsageBillingRequestConflict
		}
		id = current
	}
	if id == "" {
		return nil, nil
	}
	return s.Get(ctx, id)
}

func (s *imageTaskStore) Claim(ctx context.Context, record *service.ImageTaskRecord, ttl time.Duration) (*service.ImageTaskRecord, bool, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, false, err
	}
	p := record.Provider
	result, err := s.rdb.Eval(ctx, `local a=redis.call('GET',KEYS[1]); local b=redis.call('GET',KEYS[2]);
if a and b and a~=b then return redis.error_reply('image request aliases conflict') end
if a or b then return a or b end
redis.call('SET',KEYS[3],ARGV[1],'PX',ARGV[2]); redis.call('SET',KEYS[1],ARGV[3],'PX',ARGV[2]); redis.call('SET',KEYS[2],ARGV[3],'PX',ARGV[2]); return ''`, []string{asyncImageAlias(record.APIKeyID, "request", p.RequestID), asyncImageAlias(record.APIKeyID, "idempotency", p.IdempotencyKey), imageTaskKey(record.ID)}, string(data), ttl.Milliseconds(), record.ID).Text()
	if err != nil {
		return nil, false, err
	}
	if result == "" {
		return record, true, nil
	}
	existing, err := s.Get(ctx, result)
	return existing, false, err
}

func (s *imageTaskStore) Get(ctx context.Context, id string) (*service.ImageTaskRecord, error) {
	data, err := s.rdb.Get(ctx, imageTaskKey(id)).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, service.ErrImageTaskNotFound
		}
		return nil, err
	}
	var task service.ImageTaskRecord
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func imageTaskKey(id string) string {
	return imageTaskKeyPrefix + strings.TrimSpace(id)
}
