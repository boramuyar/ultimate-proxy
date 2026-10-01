// Package insights watches traffic for problems worth raising, starting with
// prompt-cache misses: it fingerprints each request's prompt prefix, works
// out whether the upstream should have served it from cache, explains why it
// didn't, and turns repeated problems into insights and alerts.
package insights

import (
	"encoding/json"
	"hash/maphash"
	"sort"

	"github.com/omni-proxy/omni-proxy/internal/openresponses"
)

// maxSegments caps how many input items are fingerprinted per request.
const maxSegments = 512

var seed = maphash.MakeSeed()

// Segment kinds, in prompt order.
const (
	segInstructions = iota
	segTools
	segItem
)

// Fingerprint describes a request's prompt prefix as a chain of segments:
// the instructions, the tool definitions, then each input item. Cumulative
// hashes identify "everything up to and including segment i", which is what
// a provider's prefix cache matches on. Only hashes are kept, never content.
type Fingerprint struct {
	Cumulative []uint64 // hash of segments 0..i
	CumTokens  []int    // estimated tokens in segments 0..i
	Kinds      []int
	Parts      []uint64 // hash of segment i alone

	InstructionsMasked uint64 // instructions with digits and hex runs masked
	ToolsCanonical     uint64 // tools with sorted keys, sorted by name

	TotalTokens    int
	CacheKey       string
	HasPrevious    bool // previous_response_id set: the real prefix is unknown
	HasInstrOrTool bool
}

// EstimateTokens approximates tokens from bytes, the usual ~4 bytes/token.
func EstimateTokens(n int) int { return (n + 3) / 4 }

// Compute fingerprints a raw Open Responses request body.
func Compute(body []byte) *Fingerprint {
	env, err := openresponses.ParseEnvelope(body)
	if err != nil {
		return &Fingerprint{}
	}
	return FromEnvelope(env)
}

// FromEnvelope fingerprints a parsed request. It hashes the raw bytes the
// envelope already sliced out, so the prompt is never decoded again.
func FromEnvelope(env *openresponses.Envelope) *Fingerprint {
	fp := &Fingerprint{}
	if env.PromptCacheKey != nil {
		fp.CacheKey = *env.PromptCacheKey
	}
	fp.HasPrevious = env.PreviousResponseID != nil && *env.PreviousResponseID != ""

	var h maphash.Hash
	h.SetSeed(seed)
	tokens := 0
	add := func(kind int, raw []byte) {
		h.Write(raw)
		h.WriteByte(0)
		tokens += EstimateTokens(len(raw))
		fp.Cumulative = append(fp.Cumulative, h.Sum64())
		fp.CumTokens = append(fp.CumTokens, tokens)
		fp.Kinds = append(fp.Kinds, kind)
		fp.Parts = append(fp.Parts, maphash.Bytes(seed, raw))
	}

	instr := env.Instructions
	add(segInstructions, instr)
	fp.InstructionsMasked = maphash.Bytes(seed, maskVolatile(instr))

	tools := env.Tools
	add(segTools, tools)
	fp.ToolsCanonical = canonicalTools(tools)
	fp.HasInstrOrTool = len(instr) > 0 || len(tools) > 0

	switch {
	case env.InputItems != nil:
		for i, it := range env.InputItems {
			if i == maxSegments {
				break
			}
			add(segItem, it)
		}
	case len(env.Input) > 0 && env.Input[0] != '[':
		add(segItem, env.Input)
	}
	fp.TotalTokens = tokens
	return fp
}

// maskVolatile replaces runs of digits and long hex/uuid-like runs with a
// placeholder, so two prompts that differ only in a timestamp, date, counter
// or id hash the same.
func maskVolatile(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		if isDigit(c) || (isHex(c) && hexRunLen(b[i:]) >= 8) {
			j := i
			for j < len(b) && (isHex(b[j]) || b[j] == '-' || b[j] == ':' || b[j] == '.' || b[j] == 'T' || b[j] == 'Z') {
				j++
			}
			out = append(out, '#')
			i = j
			continue
		}
		out = append(out, c)
		i++
	}
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isHex(c byte) bool   { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }

func hexRunLen(b []byte) int {
	n := 0
	for n < len(b) && (isHex(b[n]) || b[n] == '-') {
		n++
	}
	return n
}

// canonicalTools hashes tool definitions independent of key order and tool
// order, to tell "same tools, serialized differently" from "different tools".
func canonicalTools(raw []byte) uint64 {
	if len(raw) == 0 {
		return 0
	}
	var tools []any
	if json.Unmarshal(raw, &tools) != nil {
		return maphash.Bytes(seed, raw)
	}
	enc := make([]string, 0, len(tools))
	for _, t := range tools {
		b, _ := json.Marshal(t) // encoding/json sorts map keys
		enc = append(enc, string(b))
	}
	sort.Strings(enc)
	var h maphash.Hash
	h.SetSeed(seed)
	for _, e := range enc {
		h.WriteString(e)
		h.WriteByte(0)
	}
	return h.Sum64()
}
