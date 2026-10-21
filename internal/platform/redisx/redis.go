// Package redisx opens the Redis client (a cluster seed list in production).
package redisx

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// New returns a client. A comma-separated address list selects cluster mode.
func New(addr string) redis.UniversalClient {
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:        strings.Split(addr, ","),
		DialTimeout:  2 * time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		PoolSize:     100,
	})
}

func Ping(ctx context.Context, rdb redis.UniversalClient) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return rdb.Ping(ctx).Err()
}
