package openresponses

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"github.com/go-json-experiment/json/jsontext"
)

// Envelope is what the proxy reads from a request body, in a single pass: the
// few fields it acts on, plus the byte ranges of the prompt so the cache
// fingerprint can hash them without decoding them. Everything else goes to
// the upstream exactly as the client sent it.
type Envelope struct {
	Model              string
	Stream             bool
	Background         bool
	PromptCacheKey     *string
	PreviousResponseID *string
	SafetyIdentifier   *string
	Metadata           map[string]string

	// Raw JSON values sliced from the body; nil when absent or null.
	Instructions []byte
	Tools        []byte
	Input        []byte
	InputItems   [][]byte // the elements of Input when it is an array

	body     []byte
	objStart int  // offset just after the opening brace
	model    span // where the model value sits in body
	stream   span
}

type span struct{ start, end int }

func (s span) ok() bool { return s.end > 0 }

// ParseEnvelope validates body as JSON and reads its envelope. Top-level keys
// match exactly and may not repeat: the upstream body is spliced rather than
// re-encoded, so the proxy and the upstream must read the same model.
func ParseEnvelope(body []byte) (*Envelope, *APIError) {
	e := &Envelope{body: body}
	// Nested duplicates and invalid UTF-8 are passed through, as encoding/json
	// always did; only the top level is checked, below.
	d := jsontext.NewDecoder(bytes.NewBuffer(body), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if tok, err := d.ReadToken(); err != nil {
		return nil, syntaxError(err)
	} else if tok.Kind() != '{' {
		return nil, InvalidRequest("invalid_json", "Request body must be a JSON object.", "")
	}
	e.objStart = int(d.InputOffset())
	seen := make(map[string]struct{}, 16)
	for d.PeekKind() != '}' {
		tok, err := d.ReadToken()
		if err != nil {
			return nil, syntaxError(err)
		}
		key := tok.String()
		if _, dup := seen[key]; dup {
			return nil, InvalidRequest("invalid_json", "Request body has a duplicate "+strconv.Quote(key)+" field.", key)
		}
		seen[key] = struct{}{}

		if key == "input" && d.PeekKind() == '[' {
			if err := e.readItems(d); err != nil {
				return nil, syntaxError(err)
			}
			continue
		}
		v, err := d.ReadValue()
		if err != nil {
			return nil, syntaxError(err)
		}
		end := int(d.InputOffset())
		sp := span{end - len(v), end}
		raw := body[sp.start:sp.end] // v is only valid until the next read
		if apiErr := e.set(key, raw, sp); apiErr != nil {
			return nil, apiErr
		}
	}
	if _, err := d.ReadToken(); err != nil {
		return nil, syntaxError(err)
	}
	if _, err := d.ReadToken(); err != io.EOF {
		return nil, InvalidRequest("invalid_json", "Request body has data after the JSON object.", "")
	}
	return e, nil
}

func (e *Envelope) readItems(d *jsontext.Decoder) error {
	if _, err := d.ReadToken(); err != nil { // [
		return err
	}
	start := int(d.InputOffset()) - 1
	for d.PeekKind() != ']' {
		v, err := d.ReadValue()
		if err != nil {
			return err
		}
		end := int(d.InputOffset())
		e.InputItems = append(e.InputItems, e.body[end-len(v):end])
	}
	if _, err := d.ReadToken(); err != nil { // ]
		return err
	}
	e.Input = e.body[start:d.InputOffset()]
	return nil
}

func (e *Envelope) set(key string, raw []byte, sp span) *APIError {
	isNull := string(raw) == "null"
	var err error
	switch key {
	case "model":
		e.model = sp
		if !isNull {
			e.Model, err = unquote(raw)
		}
	case "stream":
		e.Stream, err = boolean(raw)
		e.stream = sp
	case "background":
		e.Background, err = boolean(raw)
	case "prompt_cache_key":
		e.PromptCacheKey, err = optString(raw)
	case "previous_response_id":
		e.PreviousResponseID, err = optString(raw)
	case "safety_identifier":
		e.SafetyIdentifier, err = optString(raw)
	case "metadata":
		err = json.Unmarshal(raw, &e.Metadata)
	case "instructions":
		if !isNull {
			e.Instructions = raw
		}
	case "tools":
		if !isNull {
			e.Tools = raw
		}
	case "input":
		if !isNull {
			e.Input = raw
		}
	}
	if err != nil {
		return InvalidRequest("invalid_type", "Invalid type for '"+key+"': "+err.Error(), key)
	}
	return nil
}

var errWantString = errors.New("expected a string")

func unquote(raw []byte) (string, error) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", errWantString
	}
	s, err := jsontext.AppendUnquote(nil, raw)
	return string(s), err
}

func optString(raw []byte) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	s, err := unquote(raw)
	return &s, err
}

func boolean(raw []byte) (bool, error) {
	switch string(raw) {
	case "true":
		return true, nil
	case "false", "null":
		return false, nil
	}
	return false, errors.New("expected a boolean")
}

func syntaxError(err error) *APIError {
	return InvalidRequest("invalid_json", "Request body is not valid JSON: "+err.Error(), "")
}

// UpstreamBody returns the body with model replaced and stream forced to
// true, spliced into the client's bytes rather than re-encoded.
func (e *Envelope) UpstreamBody(model string) []byte {
	qm, _ := jsontext.AppendQuote(nil, model)
	type edit struct {
		span
		text []byte
	}
	var edits []edit
	var prefix []byte // new members inserted after the opening brace
	if e.model.ok() {
		edits = append(edits, edit{e.model, qm})
	} else {
		prefix = append(append(append(prefix, `"model":`...), qm...), ',')
	}
	if e.stream.ok() {
		edits = append(edits, edit{e.stream, []byte("true")})
	} else {
		prefix = append(prefix, `"stream":true,`...)
	}
	if len(edits) == 2 && edits[1].start < edits[0].start {
		edits[0], edits[1] = edits[1], edits[0]
	}
	if len(prefix) > 0 && bytes.TrimSpace(e.body[e.objStart:])[0] == '}' {
		prefix = prefix[:len(prefix)-1] // empty object: no trailing comma
	}

	out := make([]byte, 0, len(e.body)+len(prefix)+len(qm)+8)
	out = append(out, e.body[:e.objStart]...)
	out = append(out, prefix...)
	i := e.objStart
	for _, ed := range edits {
		out = append(out, e.body[i:ed.start]...)
		out = append(out, ed.text...)
		i = ed.end
	}
	return append(out, e.body[i:]...)
}

// Request decodes the whole body, for adapters that translate the request
// rather than pass it through.
func (e *Envelope) Request() (*Request, *APIError) {
	var r Request
	if err := json.Unmarshal(e.body, &r); err != nil {
		return nil, InvalidRequest("invalid_type", err.Error(), "")
	}
	return &r, nil
}
