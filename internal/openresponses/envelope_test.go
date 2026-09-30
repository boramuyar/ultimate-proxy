package openresponses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseEnvelope(t *testing.T) {
	body := []byte(` {"input":[{"type":"message","role":"user","content":"a\"b"} , "x"],"model":"gpt","instructions":"be helpful",
		"tools":[{"type":"function","name":"f"}],"prompt_cache_key":"k","previous_response_id":null,
		"metadata":{"user_email":"a@b.c"},"safety_identifier":"s","background":false,"temperature":0.2} `)
	e, err := ParseEnvelope(body)
	if err != nil {
		t.Fatal(err)
	}
	if e.Model != "gpt" || e.Stream || e.Background || *e.PromptCacheKey != "k" || e.PreviousResponseID != nil ||
		*e.SafetyIdentifier != "s" || e.Metadata["user_email"] != "a@b.c" {
		t.Fatalf("fields: %+v", e)
	}
	if string(e.Instructions) != `"be helpful"` || string(e.Tools) != `[{"type":"function","name":"f"}]` {
		t.Fatalf("raw: %s %s", e.Instructions, e.Tools)
	}
	if len(e.InputItems) != 2 || string(e.InputItems[0]) != `{"type":"message","role":"user","content":"a\"b"}` || string(e.InputItems[1]) != `"x"` {
		t.Fatalf("items: %q", e.InputItems)
	}
	if !strings.HasPrefix(string(e.Input), "[{") || !strings.HasSuffix(string(e.Input), `"x"]`) {
		t.Fatalf("input: %s", e.Input)
	}
}

func TestParseEnvelopeRejects(t *testing.T) {
	for body, code := range map[string]string{
		`{"model":"a","model":"b"}`:                  "invalid_json",
		`{"model":"a","stream":true,"stream":false}`: "invalid_json",
		`{"model":"a",}`:                             "invalid_json",
		`{"model":"a"} {}`:                           "invalid_json",
		`{"input":[1,]}`:                             "invalid_json",
		`{"a":"\x"}`:                                 "invalid_json",
		`{"a":tru}`:                                  "invalid_json",
		`[]`:                                         "invalid_json",
		``:                                           "invalid_json",
		`{"model":5}`:                                "invalid_type",
		`{"stream":"yes"}`:                           "invalid_type",
		`{"metadata":{"a":1}}`:                       "invalid_type",
		`{"prompt_cache_key":false}`:                 "invalid_type",
	} {
		_, err := ParseEnvelope([]byte(body))
		if err == nil {
			t.Errorf("accepted %s", body)
		} else if err.CodeString() != code {
			t.Errorf("%s: code %s, want %s", body, err.CodeString(), code)
		}
	}
}

func TestUpstreamBody(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"model":"gpt","input":"hi"}`, `{"model":"up","input":"hi","stream":true}`},
		{`{"stream":false,"input":"hi","model":"gpt"}`, `{"stream":true,"input":"hi","model":"up"}`},
		{`{"input":"hi"}`, `{"model":"up","stream":true,"input":"hi"}`},
		{`{ }`, `{"model":"up","stream":true}`},
		{`{"model":null}`, `{"model":"up","stream":true}`},
	} {
		e, err := ParseEnvelope([]byte(tc.in))
		if err != nil {
			t.Fatal(tc.in, err)
		}
		got := e.UpstreamBody("up")
		if !json.Valid(got) || !sameJSON(t, got, []byte(tc.want)) {
			t.Errorf("%s: got %s, want %s", tc.in, got, tc.want)
		}
	}
	// Bytes outside the spliced values are left exactly as sent.
	in := `{"model": "gpt" , "input":"café 😀", "x": 1.50e2}`
	e, _ := ParseEnvelope([]byte(in))
	if got := string(e.UpstreamBody("up")); got != `{"model": "up" , "stream":true,"input":"café 😀", "x": 1.50e2}` &&
		got != `{"stream":true,"model": "up" , "input":"café 😀", "x": 1.50e2}` {
		t.Errorf("got %s", got)
	}
}

func sameJSON(t testing.TB, a, b []byte) bool {
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("%s: %v", a, err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

// FuzzParseEnvelope checks the one-pass parser against encoding/json.
func FuzzParseEnvelope(f *testing.F) {
	for _, s := range []string{
		`{"model":"gpt","input":"hi"}`,
		`{"model":"gpt","stream":false,"input":[{"type":"message","content":[{"type":"input_text","text":"a\nb"}]},"x",1,null]}`,
		`{"instructions":null,"tools":[],"metadata":{"user_email":"x"},"input":[]}`,
		`{"model":"a","model":"b"}`,
		`{"Model":"a","model":"b","stream":null}`,
		`{"model":"é\"","prompt_cache_key":"k","previous_response_id":"p"}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		e, apiErr := ParseEnvelope(body)
		valid := json.Valid(body)
		if apiErr != nil {
			// Anything encoding/json accepts as an object is only refused for
			// a duplicate top-level key or a field of the wrong type.
			var m map[string]json.RawMessage
			if valid && json.Unmarshal(body, &m) == nil && m != nil && apiErr.CodeString() == "invalid_json" &&
				!strings.Contains(apiErr.Message, "duplicate") {
				t.Fatalf("rejected valid object %q: %v", body, apiErr)
			}
			return
		}
		if !valid {
			t.Fatalf("accepted invalid JSON %q", body)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil || m == nil {
			t.Fatalf("accepted non-object %q", body)
		}
		str := func(k string) string {
			var s string
			_ = json.Unmarshal(m[k], &s)
			return s
		}
		if e.Model != str("model") {
			t.Fatalf("model %q vs %q", e.Model, str("model"))
		}
		if raw, ok := m["input"]; ok && bytes.HasPrefix(raw, []byte("[")) {
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil || len(items) != len(e.InputItems) {
				t.Fatalf("items: %d vs %d", len(items), len(e.InputItems))
			}
			for i := range items {
				if !bytes.Equal(items[i], e.InputItems[i]) {
					t.Fatalf("item %d: %s vs %s", i, items[i], e.InputItems[i])
				}
			}
		}
		up := e.UpstreamBody("up-model")
		var got map[string]json.RawMessage
		if err := json.Unmarshal(up, &got); err != nil {
			t.Fatalf("upstream body %q invalid: %v", up, err)
		}
		m["model"] = json.RawMessage(`"up-model"`)
		m["stream"] = json.RawMessage(`true`)
		if fmt.Sprint(canon(t, m)) != fmt.Sprint(canon(t, got)) {
			t.Fatalf("upstream body differs:\n%s\n%s", body, up)
		}
	})
}

func canon(t *testing.T, m map[string]json.RawMessage) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(x)
		out[k] = string(b)
	}
	return out
}
