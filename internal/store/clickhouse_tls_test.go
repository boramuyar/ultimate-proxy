package store

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClickHouseCA checks that ?sslrootcert= makes the proxy trust a
// ClickHouse whose certificate comes from a private CA.
func TestClickHouseCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("1\n")) // the database exists; every other answer is ignored
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	base := "https://proxy:pw@" + strings.TrimPrefix(srv.URL, "https://") + "/proxy"

	if _, err := NewClickHouse(context.Background(), base, 0); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without the CA: err = %v, want a certificate error", err)
	}
	ch, err := NewClickHouse(context.Background(), base+"?sslrootcert="+url.QueryEscape(ca), 0)
	if err != nil {
		t.Fatal(err)
	}
	ch.Close()
	if _, err := NewClickHouse(context.Background(), base+"?sslrootcert=/nonexistent.pem", 0); err == nil {
		t.Fatal("a missing CA file should fail")
	}
}
