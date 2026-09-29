package server

import (
	"net/http"
	"strings"
	"testing"
)

// BenchmarkOverhead compares a request sent straight to the fake upstream
// with the same request through the proxy (auth, translation, metering). The
// difference is the proxy's added latency.
func BenchmarkOverhead(b *testing.B) {
	h := newHarness(b)

	anth := `{"model":"claude-fake","max_tokens":100,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi there"}]}]}`
	upURL := h.upstreamURL + "/v1/messages"
	b.Run("direct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			req, _ := http.NewRequest("POST", upURL, strings.NewReader(anth))
			req.Header.Set("x-api-key", "fake-key")
			req.Header.Set("anthropic-version", "2023-06-01")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			drain(resp)
		}
	})
	b.Run("proxy", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			drain(h.post(trustedKey, `{"model":"claude","input":"hi there"}`))
		}
	})
	b.Run("proxy-stream", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			drain(h.post(trustedKey, `{"model":"claude","input":"hi there","stream":true}`))
		}
	})
}

func drain(resp *http.Response) {
	buf := make([]byte, 32*1024)
	for {
		if _, err := resp.Body.Read(buf); err != nil {
			break
		}
	}
	resp.Body.Close()
}
