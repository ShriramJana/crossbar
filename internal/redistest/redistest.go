// Package redistest connects tests to a real Redis. The limiter's atomicity
// lives in a Lua script, so an in-process stand-in would test a reimplementation
// rather than the code that ships. Tests that need Redis skip when none is
// reachable; `make redis` starts one.
package redistest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Addr returns the Redis address under test, from REDIS_ADDR or the local default.
func Addr() string {
	if a := os.Getenv("REDIS_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:6379"
}

// Client returns a connected client or skips the test when Redis is unreachable.
// Keys written under prefixes returned by UniqueID are deleted at cleanup.
func Client(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: Addr(), DialTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		t.Skipf("redis not reachable at %s (%v); start one with `make redis`", Addr(), err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// UniqueID returns an identifier unique to this test run so concurrent test
// binaries sharing one Redis never touch each other's keys. Keys containing
// the ID are deleted when the test ends.
func UniqueID(t *testing.T, rdb *redis.Client) string {
	t.Helper()
	var b [6]byte
	_, _ = rand.Read(b[:])
	id := "t" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		iter := rdb.Scan(ctx, 0, "*"+id+"*", 100).Iterator()
		for iter.Next(ctx) {
			_ = rdb.Del(ctx, iter.Val()).Err()
		}
	})
	return id
}
