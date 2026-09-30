package limits

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
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
	// request limits, 0 for token limits, which are charged afterwards.
	Take int64
}

// Counter keeps sliding-minute counters.
type Counter interface {
	// Take checks every window and, only if each has room for one more
	// unit, adds each window's Take. It returns the index of the first full
	// window (-1 if none) and each window's use before this request.
	Take(ctx context.Context, ws []Window, now time.Time) (blocked int, used []float64, err error)
	// Add charges n units to each key, for token limits after a response.
	Add(ctx context.Context, keys []string, n int64, now time.Time) error
	// Used reads a key's current use.
	Used(ctx context.Context, key string, now time.Time) (float64, error)
}

func bucket(now time.Time) (b int64, prevWeight float64) {
	ms := now.UnixMilli()
	w := window.Milliseconds()
	return ms / w, 1 - float64(ms%w)/float64(w)
}

// Memory counts in process: exact on one replica, per replica on several.
type Memory struct {
	mu     sync.Mutex
	counts map[string]int64 // key/bucket -> count
	swept  int64
}

func NewMemory() *Memory { return &Memory{counts: map[string]int64{}} }

func (m *Memory) sweep(b int64) {
	if b == m.swept {
		return
	}
	m.swept = b
	prefix := "/" + strconv.FormatInt(b, 10)
	prev := "/" + strconv.FormatInt(b-1, 10)
	for k := range m.counts {
		if !hasSuffix(k, prefix) && !hasSuffix(k, prev) {
			delete(m.counts, k)
		}
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func (m *Memory) used(key string, b int64, pw float64) float64 {
	return float64(m.counts[fmt.Sprintf("%s/%d", key, b-1)])*pw + float64(m.counts[fmt.Sprintf("%s/%d", key, b)])
}

func (m *Memory) Take(_ context.Context, ws []Window, now time.Time) (int, []float64, error) {
	b, pw := bucket(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(b)
	used := make([]float64, len(ws))
	blocked := -1
	for i, w := range ws {
		used[i] = m.used(w.Key, b, pw)
		if blocked < 0 && used[i]+1 > w.Limit {
			blocked = i
		}
	}
	if blocked < 0 {
		for _, w := range ws {
			if w.Take != 0 {
				m.counts[fmt.Sprintf("%s/%d", w.Key, b)] += w.Take
			}
		}
	}
	return blocked, used, nil
}

func (m *Memory) Add(_ context.Context, keys []string, n int64, now time.Time) error {
	b, _ := bucket(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(b)
	for _, k := range keys {
		m.counts[fmt.Sprintf("%s/%d", k, b)] += n
	}
	return nil
}

func (m *Memory) Used(_ context.Context, key string, now time.Time) (float64, error) {
	b, pw := bucket(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used(key, b, pw), nil
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

// KEYS: current and previous bucket of each window, in pairs.
// ARGV: previous-bucket weight, TTL in ms, then limit and take per window.
// Returns the 1-based index of the first full window (0 if none), then each
// window's use as a string.
//
//go:embed take.lua
var takeScript string

var take = redis.NewScript(takeScript)

func (r *Redis) Take(ctx context.Context, ws []Window, now time.Time) (int, []float64, error) {
	b, pw := bucket(now)
	keys := make([]string, 0, 2*len(ws))
	args := make([]any, 0, 2+2*len(ws))
	args = append(args, strconv.FormatFloat(pw, 'f', 6, 64), (2 * window).Milliseconds())
	for _, w := range ws {
		keys = append(keys, fmt.Sprintf("%s/%d", w.Key, b), fmt.Sprintf("%s/%d", w.Key, b-1))
		args = append(args, strconv.FormatFloat(w.Limit, 'f', -1, 64), w.Take)
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

func (r *Redis) Add(ctx context.Context, keys []string, n int64, now time.Time) error {
	b, _ := bucket(now)
	p := r.c.Pipeline()
	for _, k := range keys {
		key := fmt.Sprintf("%s/%d", k, b)
		p.IncrBy(ctx, key, n)
		p.PExpire(ctx, key, 2*window)
	}
	_, err := p.Exec(ctx)
	return err
}

func (r *Redis) Used(ctx context.Context, key string, now time.Time) (float64, error) {
	b, pw := bucket(now)
	vals, err := r.c.MGet(ctx, fmt.Sprintf("%s/%d", key, b), fmt.Sprintf("%s/%d", key, b-1)).Result()
	if err != nil {
		return 0, err
	}
	num := func(v any) float64 {
		s, _ := v.(string)
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	return num(vals[1])*pw + num(vals[0]), nil
}
