// Package meter records usage events off the request path, writing them to
// the store in batches.
package meter

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/boramuyar/ultimate-proxy/internal/metrics"
	"github.com/boramuyar/ultimate-proxy/internal/store"
)

type Meter struct {
	ch        chan store.UsageEvent
	store     store.Store
	batchSize int
	interval  time.Duration
	wg        sync.WaitGroup
	log       *slog.Logger
}

// New starts a meter. Record never blocks: when queueSize events are already
// waiting, new events are dropped and counted.
func New(s store.Store, queueSize, batchSize int, interval time.Duration, log *slog.Logger) *Meter {
	m := &Meter{ch: make(chan store.UsageEvent, queueSize), store: s, batchSize: batchSize, interval: interval, log: log}
	m.wg.Add(1)
	go m.run()
	return m
}

func (m *Meter) Record(e store.UsageEvent) {
	select {
	case m.ch <- e:
	default:
		metrics.UsageDropped.Inc()
	}
}

// Close flushes queued events and stops the writer.
func (m *Meter) Close() {
	close(m.ch)
	m.wg.Wait()
}

func (m *Meter) run() {
	defer m.wg.Done()
	batch := make([]store.UsageEvent, 0, m.batchSize)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := m.store.InsertUsage(ctx, batch)
		if err != nil {
			// One retry covers brief connection blips without holding the queue long.
			err = m.store.InsertUsage(ctx, batch)
		}
		cancel()
		if err != nil {
			metrics.UsageWriteErrors.Inc()
			m.log.Error("writing usage events failed", "events", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case e, ok := <-m.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= m.batchSize {
				flush()
			}
		case <-ticker.C:
			metrics.UsageQueueDepth.Set(float64(len(m.ch)))
			flush()
		}
	}
}
