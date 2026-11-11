// Package redisx wraps Redis for caching, distributed locking, rate limiting and the
// live counters that back quota enforcement.
//
// Everything stored here must be reconstructible from Postgres. Redis is treated as a
// performance and coordination tier, never as a system of record: a flushed Redis must
// degrade the platform, not corrupt it.
package redisx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/config"
	"github.com/redis/go-redis/v9"
)

// Client wraps the Redis client with the operations this platform needs.
type Client struct {
	rdb *redis.Client
}

// Open connects to Redis and verifies reachability.
func Open(ctx context.Context, cfg config.RedisConfig) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &Client{rdb: rdb}, nil
}

// Raw exposes the underlying client for operations not wrapped here.
func (c *Client) Raw() *redis.Client { return c.rdb }

// Close releases the connection pool.
func (c *Client) Close() error { return c.rdb.Close() }

// Ping verifies connectivity for readiness probes.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.rdb.Ping(ctx).Err()
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// slidingWindowScript implements a fixed-window counter with an atomic first-hit
// expiry.
//
// A plain INCR followed by EXPIRE has a real failure mode: if the process dies between
// the two commands the key never expires and the caller is throttled forever. Doing
// both in one script removes that window entirely.
var slidingWindowScript = redis.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
local ttl = redis.call('PTTL', KEYS[1])
return {current, ttl}
`)

// Allow reports whether the key is within limit for the window, and how long until the
// window resets. It implements httpx.Limiter.
func (c *Client) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	res, err := slidingWindowScript.Run(ctx, c.rdb, []string{"rl:" + key}, window.Milliseconds()).Slice()
	if err != nil {
		return false, 0, fmt.Errorf("rate limit script: %w", err)
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("rate limit script returned %d values, want 2", len(res))
	}

	current, _ := res[0].(int64)
	ttlMS, _ := res[1].(int64)

	retryAfter := time.Duration(ttlMS) * time.Millisecond
	if ttlMS < 0 {
		// PTTL returns -1 for a key with no expiry; treat it as a full window rather
		// than as "retry immediately", which would let a caller spin.
		retryAfter = window
	}

	return current <= int64(limit), retryAfter, nil
}

// Reset clears a rate-limit key. Used after a successful sign-in so a user who
// mistyped their password a few times is not left throttled.
func (c *Client) Reset(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, "rl:"+key).Err()
}

// ---------------------------------------------------------------------------
// Distributed locks
// ---------------------------------------------------------------------------

// ErrLockNotAcquired means another holder owns the lock.
var ErrLockNotAcquired = errors.New("lock not acquired")

// Lock is a held distributed lock.
type Lock struct {
	client *Client
	key    string
	token  string
}

// releaseScript deletes the key only if we still hold it.
//
// An unconditional DEL is unsafe: if our work overran the TTL, the lock has already
// been handed to another holder, and deleting it would release *their* lock. Comparing
// the token first is what makes the lock correct under overrun.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// extendScript extends the TTL only while we still hold the lock.
var extendScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0
`)

// AcquireLock takes a lock with the given TTL, or returns ErrLockNotAcquired.
//
// The TTL must exceed the longest plausible duration of the protected work. Discovery
// runs use a generous TTL plus Extend, because a lock that expires mid-run would let a
// second worker duplicate the upstream API calls the lock exists to prevent.
func (c *Client) AcquireLock(ctx context.Context, name string, ttl time.Duration) (*Lock, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}

	key := "lock:" + name
	ok, err := c.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("acquire lock %q: %w", name, err)
	}
	if !ok {
		return nil, ErrLockNotAcquired
	}

	return &Lock{client: c, key: key, token: token}, nil
}

// Release frees the lock if still held by this holder.
func (l *Lock) Release(ctx context.Context) error {
	// Use a context that survives cancellation of the work context, otherwise a
	// cancelled job leaks its lock until the TTL expires.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return releaseScript.Run(ctx, l.client.rdb, []string{l.key}, l.token).Err()
}

// Extend renews the TTL while the lock is still held. Returns ErrLockNotAcquired if it
// has already been lost, which tells long-running work to stop rather than continue
// unprotected.
func (l *Lock) Extend(ctx context.Context, ttl time.Duration) error {
	n, err := extendScript.Run(ctx, l.client.rdb, []string{l.key}, l.token, ttl.Milliseconds()).Int64()
	if err != nil {
		return fmt.Errorf("extend lock: %w", err)
	}
	if n == 0 {
		return ErrLockNotAcquired
	}
	return nil
}

// WithLock runs fn while holding a lock, releasing it afterwards. If the lock is held
// elsewhere, fn is skipped and ErrLockNotAcquired is returned.
func (c *Client) WithLock(ctx context.Context, name string, ttl time.Duration, fn func(ctx context.Context) error) error {
	lock, err := c.AcquireLock(ctx, name, ttl)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(ctx) }()
	return fn(ctx)
}

// ---------------------------------------------------------------------------
// Counters (quota usage)
// ---------------------------------------------------------------------------

// IncrCounter increments a counter, setting its expiry atomically on first use.
func (c *Client) IncrCounter(ctx context.Context, key string, by int64, ttl time.Duration) (int64, error) {
	res, err := incrWithTTLScript.Run(ctx, c.rdb, []string{"ctr:" + key}, by, ttl.Milliseconds()).Int64()
	if err != nil {
		return 0, fmt.Errorf("increment counter %q: %w", key, err)
	}
	return res, nil
}

var incrWithTTLScript = redis.NewScript(`
local current = redis.call('INCRBY', KEYS[1], ARGV[1])
if current == tonumber(ARGV[1]) then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return current
`)

// GetCounter reads a counter, returning 0 when absent.
func (c *Client) GetCounter(ctx context.Context, key string) (int64, error) {
	v, err := c.rdb.Get(ctx, "ctr:"+key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read counter %q: %w", key, err)
	}
	return v, nil
}

// DecrCounter decrements a counter without creating it. Used to return a reservation
// when the work it was taken for fails, so a failed submission does not permanently
// consume a day's allowance.
func (c *Client) DecrCounter(ctx context.Context, key string, by int64) error {
	return decrIfExistsScript.Run(ctx, c.rdb, []string{"ctr:" + key}, by).Err()
}

var decrIfExistsScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  local v = redis.call('DECRBY', KEYS[1], ARGV[1])
  if v < 0 then
    redis.call('SET', KEYS[1], 0, 'KEEPTTL')
  end
end
return 1
`)

// ---------------------------------------------------------------------------
// Kill flags (loop pause)
// ---------------------------------------------------------------------------

// SetFlag sets a boolean flag with a TTL. Used for the loop-pause kill flag that the
// submission workers check immediately before every send, which is what makes pausing
// a loop take effect in under a second instead of at the next scheduler tick.
func (c *Client) SetFlag(ctx context.Context, name string, ttl time.Duration) error {
	return c.rdb.Set(ctx, "flag:"+name, "1", ttl).Err()
}

// ClearFlag removes a flag.
func (c *Client) ClearFlag(ctx context.Context, name string) error {
	return c.rdb.Del(ctx, "flag:"+name).Err()
}

// HasFlag reports whether a flag is set.
//
// On a Redis error it returns the error rather than false. A caller checking a pause
// flag must fail closed: treating an unreachable Redis as "not paused" would keep
// submitting after a candidate asked us to stop.
func (c *Client) HasFlag(ctx context.Context, name string) (bool, error) {
	n, err := c.rdb.Exists(ctx, "flag:"+name).Result()
	if err != nil {
		return false, fmt.Errorf("check flag %q: %w", name, err)
	}
	return n > 0, nil
}

// ---------------------------------------------------------------------------
// Single-use tokens (OAuth state, verification nonces)
// ---------------------------------------------------------------------------

// PutOnce stores a single-use value.
func (c *Client) PutOnce(ctx context.Context, key, value string, ttl time.Duration) error {
	ok, err := c.rdb.SetNX(ctx, "once:"+key, value, ttl).Result()
	if err != nil {
		return fmt.Errorf("store single-use key: %w", err)
	}
	if !ok {
		return fmt.Errorf("single-use key already exists")
	}
	return nil
}

// TakeOnce atomically reads and deletes a single-use value. Returns ("", false) when
// absent. GETDEL is atomic, which is what prevents an OAuth state or verification
// nonce from being redeemed twice by concurrent requests.
func (c *Client) TakeOnce(ctx context.Context, key string) (string, bool, error) {
	v, err := c.rdb.GetDel(ctx, "once:"+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("take single-use key: %w", err)
	}
	return v, true, nil
}
