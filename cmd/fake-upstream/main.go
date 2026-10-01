// Command fake-upstream serves a deterministic fake of the OpenAI Responses
// API for local development and CI.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/omni-proxy/omni-proxy/internal/fakeupstream"
)

func main() {
	addr := flag.String("listen", ":9090", "address to listen on")
	key := flag.String("api-key", "fake-key", "API key the fake expects")
	flag.Parse()
	log.Printf("fake upstream listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, fakeupstream.New(*key).Handler()))
}
