package limits

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Minute windows are sliding: the count is the current minute plus the
// previous minute weighted by how much of it is still inside the last 60
// seconds. That costs two counters per rule instead of a log of every
// request, and is off by at most a few percent at the minute boundary.
const window = time.Minute

// Window is one counter a request is checked against.
type Window struct {
	Key   string
	Limit float64
	// Take is added to the counter when every window has room: 1 for
	// request limits, 0 for token limits and budgets, which are charged
	// afterwards.
	Take int64
	// Fixed windows are plain counters that expire with their period, for
	// budgets; the others slide over the last minute.
	Fixed bool
}

// Charge adds N to a counter after a response.
type Charge struct {
	Key   string
	N     int64
	Fixed bool
	TTL   time.Duration // for fixed counters
}

// Counter keeps sliding-minute and per-period counters.
type Counter interface {
	// Take checks every window and, only if each has room for one more
	// unit, adds each window's Take. It returns the index of the first full
	// window (-1 if none) and each window's use before this request.
	Take(ctx context.Context, ws []Window, now time.Time) (blocked int, used []float64, err error)
	// Add applies charges and returns each counter's new total.
	Add(ctx context.Context, cs []Charge, now time.Time) ([]float64, error)
	// Set overwrites a fixed counter, when budgets are rebuilt from the
	// usage log.
	Set(ctx context.Context, key string, v int64, ttl time.Duration) error
	// Used reads a counter's current use.
	Used(ctx context.Context, key string, fixed bool, now time.Time) (float64, error)
}

func bucket(now time.Time) (b int64, prevWeight float64) {
	ms := now.UnixMilli()
	w := window.Milliseconds()
	return ms / w, 1 - float64(ms%w)/float64(w)
}

func slidingKeys(key string, b int64) (cur, prev string) {
	return fmt.Sprintf("%s/%d", key, b), fmt.Sprintf("%s/%d", key, b-1)
}

// Memory counts in process: exact on one replica, per replica on several.
type Memory struct {
	mu     sync.Mutex
	counts map[string]int64 // sliding: key/bucket -> count
	fixed  map[string]fixedCount
	swept  int64
}

type fixedCount struct {
	n       int64
	expires time.Time
}

func NewMemory() *Memory { return &Memory{counts: map[string]int64{}, fixed: map[string]fixedCount{}} }

// sweep drops sliding buckets older than the previous minute, and expired
// fixed counters, once a minute.
func (m *Memory) sweep(b int64, now time.Time) {
	if b == m.swept {
		return
	}
	m.swept = b
	cur, prev := "/"+strconv.FormatInt(b, 10), "/"+strconv.FormatInt(b-1, 10)
	for k := range m.counts {
		if !strings.HasSuffix(k, cur) && !strings.HasSuffix(k, prev) {
			delete(m.counts, k)
		}
	}
	for k, f := range m.fixed {
		if !now.Before(f.expires) {
			delete(m.fixed, k)
		}
	}
}

func (m *Memory) used(key string, fixed bool, now time.Time) float64 {
	if fixed {
		if f, ok := m.fixed[key]; ok && now.Before(f.expires) {
			return float64(f.n)
		}
		return 0
	}
	b, pw := bucket(now)
	cur, prev := slidingKeys(key, b)
	return float64(m.counts[prev])*pw + float64(m.counts[cur])
}

func (m *Memory) Take(_ context.Context, ws []Window, now time.Time) (int, []float64, error) {
	b, _ := bucket(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(b, now)
	used := make([]float64, len(ws))
	blocked := -1
	for i, w := range ws {
		used[i] = m.used(w.Key, w.Fixed, now)
		if blocked < 0 && used[i]+1 > w.Limit {
			blocked = i
		}
	}
	if blocked < 0 {
		for _, w := range ws {
			if w.Take != 0 && !w.Fixed {
				cur, _ := slidingKeys(w.Key, b)
				m.counts[cur] += w.Take
			}
		}
	}
	return blocked, used, nil
}

func (m *Memory) Add(_ context.Context, cs []Charge, now time.Time) ([]float64, error) {
	b, _ := bucket(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(b, now)
	out := make([]float64, len(cs))
	for i, c := range cs {
		if c.Fixed {
			f := m.fixed[c.Key]
			if !now.Before(f.expires) {
				f = fixedCount{expires: now.Add(c.TTL)}
			}
			f.n += c.N
			m.fixed[c.Key] = f
			out[i] = float64(f.n)
			continue
		}
		cur, _ := slidingKeys(c.Key, b)
		m.counts[cur] += c.N
		out[i] = m.used(c.Key, false, now)
	}
	return out, nil
}

func (m *Memory) Set(_ context.Context, key string, v int64, ttl time.Duration) error {
	m.mu.Lock()
	m.fixed[key] = fixedCount{n: v, expires: time.Now().Add(ttl)}
	m.mu.Unlock()
	return nil
}

func (m *Memory) Used(_ context.Context, key string, fixed bool, now time.Time) (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used(key, fixed, now), nil
}

// Redis counts in Valkey or Redis, shared by every replica. Take is one
// round trip: a script checks every window and takes from all of them
// atomically.
type Redis struct {
	c *redis.Client
}

func NewRedis(url string) (*Redis, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis_url: %w", err)
	}
	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = 500 * time.Millisecond
	opts.WriteTimeout = 500 * time.Millisecond
	return &Redis{c: redis.NewClient(opts)}, nil
}

func (r *Redis) Close() error { return r.c.Close() }

// Ping checks the connection.
func (r *Redis) Ping(ctx context.Context) error { return r.c.Ping(ctx).Err() }

// KEYS: two per window: the current and previous minute bucket, or the
// fixed counter twice.
// ARGV: previous-bucket weight, TTL in ms, then limit, take and fixed (0 or
// 1) per window.
// Returns the 1-based index of the first full window (0 if none), then each
// window's use as a string.
//
//go:embed take.lua
var takeScript string

var take = redis.NewScript(takeScript)

func (r *Redis) Take(ctx context.Context, ws []Window, now time.Time) (int, []float64, error) {
	b, pw := bucket(now)
	keys := make([]string, 0, 2*len(ws))
	args := make([]any, 0, 2+3*len(ws))
	args = append(args, strconv.FormatFloat(pw, 'f', 6, 64), (2 * window).Milliseconds())
	for _, w := range ws {
		fixed := 0
		if w.Fixed {
			keys = append(keys, w.Key, w.Key)
			fixed = 1
		} else {
			cur, prev := slidingKeys(w.Key, b)
			keys = append(keys, cur, prev)
		}
		args = append(args, strconv.FormatFloat(w.Limit, 'f', -1, 64), w.Take, fixed)
	}
	res, err := take.Run(ctx, r.c, keys, args...).Slice()
	if err != nil {
		return -1, nil, err
	}
	if len(res) != len(ws)+1 {
		return -1, nil, fmt.Errorf("limits script returned %d values for %d windows", len(res), len(ws))
	}
	blocked := int(res[0].(int64)) - 1
	used := make([]float64, len(ws))
	for i := range ws {
		s, _ := res[i+1].(string)
		used[i], _ = strconv.ParseFloat(s, 64)
	}
	return blocked, used, nil
}

func (r *Redis) Add(ctx context.Context, cs []Charge, now time.Time) ([]float64, error) {
	b, _ := bucket(now)
	p := r.c.Pipeline()
	incr := make([]*redis.IntCmd, len(cs))
	for i, c := range cs {
		key, ttl := c.Key, c.TTL
		if !c.Fixed {
			key, _ = slidingKeys(c.Key, b)
			ttl = 2 * window
		}
		incr[i] = p.IncrBy(ctx, key, c.N)
		if c.Fixed {
			p.ExpireNX(ctx, key, ttl) // the period's end, set once
		} else {
			p.PExpire(ctx, key, ttl)
		}
	}
	if _, err := p.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([]float64, len(cs))
	for i := range cs {
		out[i] = float64(incr[i].Val())
	}
	return out, nil
}

func (r *Redis) Set(ctx context.Context, key string, v int64, ttl time.Duration) error {
	return r.c.Set(ctx, key, v, ttl).Err()
}

func (r *Redis) Used(ctx context.Context, key string, fixed bool, now time.Time) (float64, error) {
	num := func(v any) float64 {
		s, _ := v.(string)
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	if fixed {
		v, err := r.c.Get(ctx, key).Result()
		if err == redis.Nil {
			return 0, nil
		}
		return num(v), err
	}
	b, pw := bucket(now)
	cur, prev := slidingKeys(key, b)
	vals, err := r.c.MGet(ctx, cur, prev).Result()
	if err != nil {
		return 0, err
	}
	return num(vals[1])*pw + num(vals[0]), nil
}
