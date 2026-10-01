package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/omni-proxy/omni-proxy/internal/fakeupstream"
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

// BenchmarkLongChat measures the same for the agent-sized prompts that are
// normal traffic: every byte of the prompt passes through the proxy.
func BenchmarkLongChat(b *testing.B) {
	h := newHarness(b)
	for _, n := range []int{400 << 10, 4 << 20} {
		viaProxy := string(fakeupstream.LongChat("gpt", n))
		direct := string(fakeupstream.LongChat("gpt-fake", n))
		name := fmt.Sprintf("%dk-tokens", len(viaProxy)/4000)
		b.Run(name+"/direct", func(b *testing.B) {
			b.SetBytes(int64(len(direct)))
			for i := 0; i < b.N; i++ {
				req, _ := http.NewRequest("POST", h.upstreamURL+"/v1/responses", strings.NewReader(direct))
				req.Header.Set("Authorization", "Bearer fake-key")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					b.Fatal(err)
				}
				drain(resp)
			}
		})
		b.Run(name+"/proxy", func(b *testing.B) {
			b.SetBytes(int64(len(viaProxy)))
			for i := 0; i < b.N; i++ {
				drain(h.post(trustedKey, viaProxy))
			}
		})
	}
}
