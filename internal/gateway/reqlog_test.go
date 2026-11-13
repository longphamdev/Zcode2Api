package gateway

import (
	"strings"
	"testing"
)

// The origin reports cache counters on message_delta, not on message_start:
//
//	message_start  {"usage":{"input_tokens":0,"output_tokens":0}}          ← no cache fields
//	message_delta  {"usage":{"input_tokens":16,"output_tokens":151,
//	                         "cache_read_input_tokens":1344,...}}          ← cache here
//
// The scanner must read whichever event carries the counter, and an event that
// omits a field must not overwrite a value captured earlier.
func TestUsageScannerReadsCacheFromMessageDelta(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n",
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":1344,"cache_creation_input_tokens":0}}` + "\n\n",
		`data: {"type":"message_stop"}` + "\n\n",
	}, "")

	var s usageScanner
	if _, err := s.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := s.Usage()
	if got.CacheRead != 1344 {
		t.Errorf("CacheRead = %d, want 1344", got.CacheRead)
	}
	if got.In != 10 {
		t.Errorf("In = %d, want 10 (a later event must not clobber it to zero)", got.In)
	}
	if got.Out != 3 {
		t.Errorf("Out = %d, want 3", got.Out)
	}
}

// A later event that omits a field must not zero out what an earlier event
// reported. This is the clobbering half of the bug: message_start used to
// assign the cache counters unconditionally, so an origin that sends them
// earlier than this one would lose them.
func TestUsageScannerAbsentFieldDoesNotClobber(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":42,"cache_read_input_tokens":900}}}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}` + "\n\n",
	}, "")

	var s usageScanner
	if _, err := s.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := s.Usage()
	if got.CacheRead != 900 {
		t.Errorf("CacheRead = %d, want 900 — an event without the field must not zero it", got.CacheRead)
	}
	if got.In != 42 {
		t.Errorf("In = %d, want 42 — an event without input_tokens must not zero it", got.In)
	}
	if got.Out != 7 {
		t.Errorf("Out = %d, want 7", got.Out)
	}
}

// The origin sends exactly one message_delta carrying cumulative totals, so the
// scanner assigns rather than accumulates. Assert it: a single delta reports
// 151 output tokens, not double that.
func TestUsageScannerTreatsCountersAsCumulative(t *testing.T) {
	stream := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":16,"output_tokens":151,"cache_read_input_tokens":1344}}` + "\n\n"

	var s usageScanner
	if _, err := s.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := s.Usage()
	if got.Out != 151 {
		t.Errorf("Out = %d, want 151 (cumulative snapshot, not accumulated)", got.Out)
	}
	if got.In != 16 {
		t.Errorf("In = %d, want 16", got.In)
	}
	if got.CacheRead != 1344 {
		t.Errorf("CacheRead = %d, want 1344", got.CacheRead)
	}
}

// A stream split across several Write calls — the scanner buffers partial
// lines, so a split must not lose the usage event.
func TestUsageScannerHandlesSplitWrites(t *testing.T) {
	first := `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,`
	second := `"output_tokens":9,"cache_read_input_tokens":256}}` + "\n\n"

	var s usageScanner
	if _, err := s.Write([]byte(first)); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := s.Write([]byte(second)); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	got := s.Usage()
	if got.CacheRead != 256 {
		t.Errorf("CacheRead = %d, want 256 after a split write", got.CacheRead)
	}
	if got.Out != 9 {
		t.Errorf("Out = %d, want 9", got.Out)
	}
}

// The plain-JSON fallback (origin answered with a complete message document
// instead of SSE) must report cache counters too.
func TestUsageScannerJSONFallbackReadsCache(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant",` +
		`"usage":{"input_tokens":11,"output_tokens":4,"cache_read_input_tokens":1344,"cache_creation_input_tokens":12}}`

	var s usageScanner
	if _, err := s.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := s.Usage()
	if got.CacheRead != 1344 {
		t.Errorf("CacheRead = %d, want 1344", got.CacheRead)
	}
	if got.CacheCreate != 12 {
		t.Errorf("CacheCreate = %d, want 12", got.CacheCreate)
	}
	if got.In != 11 || got.Out != 4 {
		t.Errorf("In/Out = %d/%d, want 11/4", got.In, got.Out)
	}
}
