package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// TestPostgresConcurrentStart starts several stores at once on an empty
// schema, as replicas do on a new database; none may fail creating tables.
func TestPostgresConcurrentStart(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := NewPostgres(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("start_test_%d", time.Now().UnixNano())
	if _, err := admin.pool.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Skipf("cannot create a schema here: %v", err)
	}
	defer admin.pool.Exec(ctx, "DROP SCHEMA "+name+" CASCADE")

	u, _ := url.Parse(raw)
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pg, err := NewPostgres(ctx, u.String())
			if err != nil {
				errs <- err
				return
			}
			pg.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
