// Package sse reads and writes server-sent event streams.
package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Event is one parsed server-sent event.
type Event struct {
	Name string
	Data []byte
}

// Reader parses an event stream. It reuses its buffers, so an Event's Data is
// only valid until the next call to Next.
type Reader struct {
	s    *bufio.Scanner
	data []byte
}

func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 64*1024*1024)
	return &Reader{s: s}
}

// Next returns the next event, or io.EOF at the end of the stream.
func (r *Reader) Next() (Event, error) {
	var ev Event
	r.data = r.data[:0]
	hasData := false
	for r.s.Scan() {
		line := r.s.Bytes()
		if len(line) == 0 {
			if hasData || ev.Name != "" {
				ev.Data = r.data
				return ev, nil
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			ev.Name = string(value)
		case "data":
			if hasData {
				r.data = append(r.data, '\n')
			}
			r.data = append(r.data, value...)
			hasData = true
		}
	}
	if err := r.s.Err(); err != nil {
		return ev, err
	}
	if hasData {
		ev.Data = r.data
		return ev, nil
	}
	return ev, io.EOF
}

// Writer writes events to an HTTP response, sending headers lazily on the
// first event so errors before the stream starts can still use a status code.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
	buf     bytes.Buffer
}

func NewWriter(w http.ResponseWriter) *Writer {
	f, _ := w.(http.Flusher)
	return &Writer{w: w, flusher: f}
}

// Started reports whether any bytes have been sent.
func (w *Writer) Started() bool { return w.started }

func (w *Writer) start() {
	if w.started {
		return
	}
	h := w.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.w.WriteHeader(http.StatusOK)
	w.started = true
}

// Event encodes v as JSON and writes it as an event named typ.
func (w *Writer) Event(typ string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.RawEvent(typ, data)
}

// RawEvent writes already-encoded data as an event named typ.
func (w *Writer) RawEvent(typ string, data []byte) error {
	w.start()
	w.buf.Reset()
	w.buf.WriteString("event: ")
	w.buf.WriteString(typ)
	w.buf.WriteString("\ndata: ")
	w.buf.Write(data)
	w.buf.WriteString("\n\n")
	if _, err := w.w.Write(w.buf.Bytes()); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// Done writes the terminal "[DONE]" marker.
func (w *Writer) Done() error {
	w.start()
	if _, err := io.WriteString(w.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}
