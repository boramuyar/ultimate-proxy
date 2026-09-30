package fakeupstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

// LongChat builds an agent-style request of about n bytes (~4 bytes per
// token): instructions, tools, then alternating turns and tool calls.
func LongChat(model string, n int) []byte {
	chunk := strings.Repeat("Here is the file:\n```go\nfunc main() {\n\tfmt.Println(\"hi, \\\"you\\\"\")\n}\n```\nNext, the <router> — ünïcode.\n", 20)
	var tools []map[string]any
	for i := 0; i < 20; i++ {
		tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("tool_%d", i), "description": strings.Repeat("Does a thing. ", 10),
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}})
	}
	var input []any
	for i, size := 0, 0; size < n; i++ {
		size += len(chunk) + 80
		if i%5 == 4 {
			id := fmt.Sprintf("call_%d", i)
			input = append(input, map[string]any{"type": "function_call", "call_id": id, "name": "tool_1", "arguments": `{"path":"a.go"}`},
				map[string]any{"type": "function_call_output", "call_id": id, "output": chunk})
			continue
		}
		role := []string{"user", "assistant"}[i%2]
		input = append(input, map[string]any{"type": "message", "role": role, "content": []map[string]any{{"type": "input_text", "text": chunk}}})
	}
	b, _ := json.Marshal(map[string]any{"model": model, "instructions": strings.Repeat("You are a coding agent. ", 300),
		"tools": tools, "input": input, "prompt_cache_key": "session-1"})
	return b
}
