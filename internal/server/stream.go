package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const maxSSELineBytes = 8 << 20

// maxEventBufBytes bounds the buffered bytes of one SSE event. Events flush
// at blank-line boundaries, but an upstream that sends data lines forever
// without one would otherwise grow the buffer without limit while the
// client sees nothing. Past this cap the partial event is flushed as-is —
// byte-identical passthrough, since the client parser also waits for the
// blank line — so memory stays bounded without ever aborting a healthy
// stream for sending a legitimately large event.
const maxEventBufBytes = 2 << 20

// maxPost200Duration is the absolute ceiling on a healthy upstream response
// once a 200 is in hand. The retry budget (correctly) stops at that point
// and the idle reader only bounds gaps between reads, so without this a
// slow-drip upstream (one byte per idle window) could pin an in-flight
// slot forever. Ten minutes is far beyond any legitimate generation —
// production long-tail requests finish in tens of seconds — while still
// bounding the drip.
const maxPost200Duration = 10 * time.Minute

// Matches balance / rate-limit style errors in a 400 body so we can fail over.
type committedStreamWriter struct {
	http.ResponseWriter
	response     *http.Response
	requestID    string
	idle         time.Duration
	beforeCommit func() error
	committed    bool
}

func (w *committedStreamWriter) Write(p []byte) (int, error) {
	if !w.committed {
		if err := w.beforeCommit(); err != nil {
			return 0, err
		}
		copyResponseHeaders(w.ResponseWriter, w.response, true)
		w.Header().Set("X-Request-ID", w.requestID)
		w.ResponseWriter.WriteHeader(http.StatusOK)
		w.committed = true
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.idle))
	return w.ResponseWriter.Write(p)
}

func (w *committedStreamWriter) FlushError() error {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.idle))
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// Non-stream responses are buffered before anything is written to the
// client (usage must be extracted and failover must still be possible).
// Holding up to 64 MiB per in-flight request in RAM does not scale, so
// past nonStreamMemLimit the tail spills to a temp file. The hard ceiling
// is unchanged: oversized or invalid bodies are still rejected.
const maxNonStreamBody = int64(64 << 20)

var nonStreamMemLimit = int64(8 << 20) // var so tests can shrink it

// spillTempDir is where oversized non-stream bodies spill. It must be
// DISK-backed: /tmp is tmpfs (RAM) on typical hosts, including the
// production box, so spilling there would defeat the point. Set once at
// server construction to the analytics DB's directory; empty falls back
// to os.TempDir.
var spillTempDir string

func setSpillTempDir(dir string) { spillTempDir = dir }

func createSpillFile() (*os.File, error) {
	if spillTempDir != "" {
		if f, err := os.CreateTemp(spillTempDir, "verloc-body-*"); err == nil {
			return f, nil
		}
	}
	return os.CreateTemp("", "verloc-body-*")
}

type spillBody struct {
	mem      []byte // prefix, up to nonStreamMemLimit
	file     *os.File
	fileSize int64
	size     int64
}

func readSpillBody(r io.Reader) (*spillBody, error) {
	b := &spillBody{}
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			data := chunk[:n]
			b.size += int64(n)
			if b.file == nil {
				room := nonStreamMemLimit - int64(len(b.mem))
				if int64(len(data)) <= room {
					b.mem = append(b.mem, data...)
				} else {
					// This chunk crosses the memory limit: mem keeps the
					// prefix, the remainder starts the spill file.
					b.mem = append(b.mem, data[:int(room)]...)
					f, ferr := createSpillFile()
					if ferr != nil {
						return b, ferr
					}
					if _, ferr = f.Write(data[int(room):]); ferr != nil {
						_ = f.Close()
						_ = os.Remove(f.Name())
						return b, ferr
					}
					b.file = f
					b.fileSize = int64(len(data)) - room
				}
			} else {
				if _, werr := b.file.Write(data); werr != nil {
					return b, werr
				}
				b.fileSize += int64(n)
			}
		}
		if err == io.EOF {
			return b, nil
		}
		if err != nil {
			return b, err
		}
	}
}

// reader streams the whole body without materializing it.
func (b *spillBody) reader() io.Reader {
	if b.file == nil {
		return bytes.NewReader(b.mem)
	}
	return io.MultiReader(bytes.NewReader(b.mem), io.NewSectionReader(b.file, 0, b.fileSize))
}

// bytes materializes the whole body. Callers that rewrite bytes use it;
// the common spilled path streams instead.
func (b *spillBody) bytes() ([]byte, error) {
	if b.file == nil {
		return b.mem, nil
	}
	out := make([]byte, 0, b.size)
	out = append(out, b.mem...)
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	tail, err := io.ReadAll(b.file)
	if err != nil {
		return nil, err
	}
	return append(out, tail...), nil
}

func (b *spillBody) validJSON() bool {
	if b.file == nil {
		return json.Valid(b.mem)
	}
	// Stream tokens instead of decoding: Decode(&any) would materialize
	// the entire JSON tree (measured +611 MB heap on a 52 MB body),
	// defeating the spill. A token drain validates syntax in O(depth)
	// memory. It must also reject trailing content after the top-level
	// value, which json.Valid does and a naive token loop would not.
	dec := json.NewDecoder(b.reader())
	dec.UseNumber() // Token must not range-check numbers into float64
	depth := 0
	completed := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return completed && depth == 0
		}
		if err != nil || completed {
			return false
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					completed = true
				}
			}
		} else if depth == 0 {
			completed = true // scalar top-level value
		}
	}
}

func (b *spillBody) close() {
	if b.file != nil {
		name := b.file.Name()
		_ = b.file.Close()
		_ = os.Remove(name)
		b.file = nil
	}
}

type idleReader struct {
	body     io.ReadCloser
	timer    *time.Timer
	timeout  time.Duration
	timedOut atomic.Bool
}

func newIdleReader(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleReader {
	r := &idleReader{body: body, timeout: timeout}
	r.timer = time.AfterFunc(timeout, func() {
		r.timedOut.Store(true)
		cancel()
		_ = body.Close()
	})
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (r *idleReader) Close() error { r.timer.Stop(); return r.body.Close() }

type streamClientError struct{ err error }

func (e *streamClientError) Error() string { return "client stream write failed" }
func (e *streamClientError) Unwrap() error { return e.err }

// streamOptions tunes per-chunk work. The zero value is the correct default
// for a stripping backend: no reasoning alias, no accumulator.
type streamOptions struct {
	// aliasReasoning copies delta.reasoning onto reasoning_content so clients
	// that only read the DeepSeek field still see thinking. Only needed when
	// the backend does NOT strip reasoning; running it unconditionally cost a
	// full JSON decode per chunk for a field that was about to be discarded.
	aliasReasoning bool
	// acc collects reasoning + tool_call ids for the replay cache. nil when the
	// backend strips reasoning.
	acc *sseReasoningAcc
}

// usageMarker gates the per-chunk usage parse. Usage appears only in the final
// chunk of a stream, so scanning for the key first avoids a full JSON decode
// on every chunk. A false negative here only means the terminal usage chunk is
// missed, which is caught by the same parse on the final event anyway.
var usageMarker = []byte(`"usage"`)

// streamCopyWithUsage reports an interrupted stream unless a protocol terminal
// marker was observed. A 200 header alone is never evidence of completion.
//
// Flushing happens once per SSE event (at the blank-line boundary) rather than
// once per line: a standard `data: {...}\n\n` event is two lines, so per-line
// flushing doubled the write syscalls, the Flush calls and the
// SetWriteDeadline calls for every chunk.
func streamCopyWithUsage(w http.ResponseWriter, src io.Reader, onUsage func(in, out, cache uint64), onFirst func(), opts streamOptions) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	terminal, finished, failed := false, false, false
	flusher, _ := w.(http.Flusher)
	var eventBuf []byte
	flushEvent := func() error {
		if len(eventBuf) == 0 {
			return nil
		}
		if _, err := w.Write(eventBuf); err != nil {
			return &streamClientError{err}
		}
		eventBuf = eventBuf[:0]
		if f, ok := w.(interface{ FlushError() error }); ok {
			if err := f.FlushError(); err != nil {
				return &streamClientError{err}
			}
		} else if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		outLine := line
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				terminal = true
			} else if payload != "" {
				data := []byte(payload)
				var event struct {
					Type    string          `json:"type"`
					Error   json.RawMessage `json:"error"`
					Choices []struct {
						Finish *string `json:"finish_reason"`
					} `json:"choices"`
				}
				if json.Unmarshal(data, &event) == nil {
					switch event.Type {
					case "response.completed":
						terminal = true
					case "response.failed", "response.incomplete", "error":
						terminal, failed = true, true
					}
					if len(event.Error) > 0 && string(event.Error) != "null" {
						terminal, failed = true, true
					}
					for _, choice := range event.Choices {
						if choice.Finish != nil {
							finished = true
						}
					}
				}
				if onUsage != nil && bytes.Contains(data, usageMarker) {
					if in, out, cache, known := usageOf(data); known {
						onUsage(in, out, cache)
					}
				}
				// Reasoning backends used to decode this chunk up to
				// three times (terminal scan, ingest, alias). Decode once
				// into a map and share it; marshal only when the alias
				// actually changed something.
				if opts.acc != nil || opts.aliasReasoning {
					if chunk, ok := decodeChunkMap(data); ok {
						if opts.acc != nil {
							opts.acc.ingestMap(chunk)
						}
						if opts.aliasReasoning && aliasReasoningMap(chunk) {
							if rewritten, err := json.Marshal(chunk); err == nil {
								outLine = "data: " + string(rewritten)
							}
						}
					}
				}
			}
		}
		eventBuf = append(eventBuf, outLine...)
		eventBuf = append(eventBuf, '\n')
		if len(eventBuf) > maxEventBufBytes {
			if err := flushEvent(); err != nil {
				return err
			}
		}
		if onFirst != nil && strings.HasPrefix(line, "data:") {
			onFirst()
			onFirst = nil
		}
		// An empty line terminates an SSE event: that is the only point where
		// the client needs the bytes, so it is the only point we flush.
		if line == "" {
			if err := flushEvent(); err != nil {
				return err
			}
		}
		if terminal && line == "" {
			if failed {
				return errors.New("upstream stream failed")
			}
			return nil
		}
	}
	// Trailing event without a blank-line terminator (truncated, or a provider
	// that omits it) must still reach the client.
	if err := flushEvent(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if failed {
		return errors.New("upstream stream failed")
	}
	if !terminal && !finished {
		return io.ErrUnexpectedEOF
	}
	return nil
}
