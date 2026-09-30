package openresponses_test

import (
	"fmt"
	"testing"

	"github.com/boramuyar/ultimate-proxy/internal/fakeupstream"
	"github.com/boramuyar/ultimate-proxy/internal/openresponses"
)

func BenchmarkParseEnvelope(b *testing.B) {
	for _, n := range []int{400 << 10, 4 << 20} {
		body := fakeupstream.LongChat("gpt", n)
		b.Run(fmt.Sprintf("%dk-tokens", len(body)/4000), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				e, err := openresponses.ParseEnvelope(body)
				if err != nil {
					b.Fatal(err)
				}
				_ = e.UpstreamBody("gpt-upstream")
			}
		})
	}
}
