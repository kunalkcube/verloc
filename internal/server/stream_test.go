package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Spilled-body JSON validation must be a streaming token drain: it may
// never materialize the value tree (that is the memory the spill exists
// to save), and it must reject trailing content like json.Valid does.
func TestSpillValidJSON(t *testing.T) {
	old := nonStreamMemLimit
	nonStreamMemLimit = 64
	defer func() { nonStreamMemLimit = old }()
	cases := map[string]bool{
		`{"a":1}`:                                true,
		`{"a":[1,2,{"b":"x"}]}`:                  true,
		`"scalar"`:                               true,
		`12345`:                                  true,
		`null`:                                   true,
		`{"a":1} {"b":2}`:                        false, // trailing second value
		`{"a":1} garbage`:                        false,
		`{"a":1`:                                 false,
		``:                                       false,
		`{"a":` + strings.Repeat("9", 500) + `}`: true, // spans the spill split
	}
	for body, want := range cases {
		sb, err := readSpillBody(strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if sb.file == nil && len(body) > 64 {
			t.Fatalf("%q did not spill; test is not exercising the spilled path", body[:20])
		}
		if got := sb.validJSON(); got != want {
			t.Errorf("validJSON(%q…) = %v, want %v", body[:min(24, len(body))], got, want)
		}
		sb.close()
	}
}

func TestStreamEventBufferIsBounded(t *testing.T) {
	chunk := "data: " + strings.Repeat("x", 4096) + "\n"
	src := strings.NewReader(strings.Repeat(chunk, (maxEventBufBytes/len(chunk))+10))
	rec := httptest.NewRecorder()
	_ = streamCopyWithUsage(rec, src, nil, func() {}, streamOptions{})
	if rec.Body.Len() == 0 {
		t.Fatal("oversized event was never flushed to the client")
	}
	// A complete oversized event followed by [DONE] succeeds end to end.
	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("a", 3<<20) + "\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec2 := httptest.NewRecorder()
	if err := streamCopyWithUsage(rec2, strings.NewReader(big), nil, func() {}, streamOptions{}); err != nil {
		t.Fatalf("large legit event failed: %v", err)
	}
	if rec2.Body.Len() < 3<<20 {
		t.Fatalf("large event truncated: %d bytes", rec2.Body.Len())
	}
}

// D1: scoped alert data must carry Err/Failover end to end, or model-scoped
// error_rate and any scoped failover_rate rule can never fire.
