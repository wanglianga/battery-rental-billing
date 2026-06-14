package redisx

import (
	"context"
	"fmt"
	"log"
	"time"

	"battery-rental/internal/config"

	"github.com/redis/go-redis/v9"
)

var Client *redis.Client

func Connect() error {
	Client = redis.NewClient(&redis.Options{
		Addr:         fmt.Sprintf("%s:%s", config.AppConfig.RedisHost, config.AppConfig.RedisPort),
		Password:     config.AppConfig.RedisPass,
		DB:           config.AppConfig.RedisDB,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
		PoolSize:     50,
		MinIdleConns: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := Client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}
	log.Println("[Redis] connected")
	return nil
}

type Lock struct {
	key   string
	token string
	ttl   time.Duration
}

func AcquireLock(ctx context.Context, key string, ttl time.Duration, retry ...int) (*Lock, bool, error) {
	maxRetry := 1
	if len(retry) > 0 && retry[0] > 1 {
		maxRetry = retry[0]
	}
	token := fmt.Sprintf("lock:%d", time.Now().UnixNano())
	fullKey := fmt.Sprintf("lock:%s", key)

	for i := 0; i < maxRetry; i++ {
		ok, err := Client.SetNX(ctx, fullKey, token, ttl).Result()
		if err != nil {
			return nil, false, err
		}
		if ok {
			return &Lock{key: fullKey, token: token, ttl: ttl}, true, nil
		}
		if i < maxRetry-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil, false, nil
}

func (l *Lock) Release(ctx context.Context) (bool, error) {
	script := redis.NewScript(`
		if redis.call("GET", KEYS[1]) == ARGV[1] then
			return redis.call("DEL", KEYS[1])
		else
			return 0
		end
	`)
	res, err := script.Run(ctx, Client, []string{l.key}, l.token).Int64()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

func (l *Lock) Refresh(ctx context.Context, newTTL time.Duration) (bool, error) {
	script := redis.NewScript(`
		if redis.call("GET", KEYS[1]) == ARGV[1] then
			return redis.call("EXPIRE", KEYS[1], ARGV[2])
		else
			return 0
		end
	`)
	sec := int64(newTTL / time.Second)
	res, err := script.Run(ctx, Client, []string{l.key}, l.token, sec).Int64()
	if err != nil {
		return false, err
	}
	if res == 1 {
		l.ttl = newTTL
	}
	return res == 1, nil
}

func SetNXWithTTL(ctx context.Context, key string, value interface{}, ttl time.Duration) (bool, error) {
	return Client.SetNX(ctx, key, value, ttl).Result()
}

func Get(ctx context.Context, key string) (string, error) {
	return Client.Get(ctx, key).Result()
}

func SetEX(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	return Client.Set(ctx, key, value, ttl).Err()
}

func Del(ctx context.Context, keys ...string) error {
	return Client.Del(ctx, keys...).Err()
}

func Incr(ctx context.Context, key string) (int64, error) {
	return Client.Incr(ctx, key).Result()
}
