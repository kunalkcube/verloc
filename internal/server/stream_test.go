package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

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
