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

	direct := `{"model":"gpt-fake","input":"hi there","stream":true}`
	upURL := h.upstreamURL + "/v1/responses"
	b.Run("direct", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			req, _ := http.NewRequest("POST", upURL, strings.NewReader(direct))
			req.Header.Set("Authorization", "Bearer fake-key")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			drain(resp)
		}
	})
	b.Run("proxy", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			drain(h.post(trustedKey, `{"model":"gpt","input":"hi there"}`))
		}
	})
	b.Run("proxy-stream", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			drain(h.post(trustedKey, `{"model":"gpt","input":"hi there","stream":true}`))
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
