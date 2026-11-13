package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
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
// scanner assigns rather than accumulates. Replay several cumulative snapshots
// and assert the last one wins: an accumulating implementation would report the
// sum (16+40 output tokens), which is the failure this pins.
func TestUsageScannerTreatsCountersAsCumulative(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_delta","delta":{"stop_reason":null},"usage":{"input_tokens":8,"output_tokens":16,"cache_read_input_tokens":1000}}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":null},"usage":{"input_tokens":12,"output_tokens":40,"cache_read_input_tokens":1200}}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":16,"output_tokens":151,"cache_read_input_tokens":1344}}` + "\n\n",
	}, "")

	var s usageScanner
	if _, err := s.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := s.Usage()
	if got.Out != 151 {
		t.Errorf("Out = %d, want 151 (last cumulative snapshot, not the 207 a running sum would give)", got.Out)
	}
	if got.In != 16 {
		t.Errorf("In = %d, want 16", got.In)
	}
	if got.CacheRead != 1344 {
		t.Errorf("CacheRead = %d, want 1344", got.CacheRead)
	}
}

// A present-but-zero counter still overwrites: it is an explicit snapshot value,
// not an omission. Only an absent field leaves the earlier value intact. Both
// halves matter, because the origin really does emit input_tokens:0 early in a
// stream — the danger is an event that repeats 0 later.
func TestUsageScannerPresentZeroOverwrites(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_delta","delta":{},"usage":{"input_tokens":42,"output_tokens":9,"cache_read_input_tokens":900}}` + "\n\n",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":10,"cache_read_input_tokens":0}}` + "\n\n",
	}, "")

	var s usageScanner
	if _, err := s.Write([]byte(stream)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := s.Usage()
	if got.In != 0 {
		t.Errorf("In = %d, want 0 — an explicit zero is a snapshot value, not an omission", got.In)
	}
	if got.CacheRead != 0 {
		t.Errorf("CacheRead = %d, want 0 — same rule for cache counters", got.CacheRead)
	}
	if got.Out != 10 {
		t.Errorf("Out = %d, want 10", got.Out)
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

// A refused request used to leave no trace at all: LogOK had one call site, on
// the path that reached an Origin reply, so the overload, the unsupported model
// and the captcha outage answered the client and vanished. /api/monitor
// reported zero requests for a gateway refusing every one of them, and
// SuccessRate — ok over total — read as a clean record for a pool serving
// nothing, because refused requests never entered the total.
//
// Driven through the real handlers rather than by calling LogReject: the bug
// was the missing call, so a test that calls the function directly would pass
// with every call site deleted.
func TestRefusedRequestsReachTheMonitor(t *testing.T) {
	expiredPool := func(t *testing.T) *Server {
		t.Helper()
		db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		acc, err := db.Add("jwt.a", "a")
		if err != nil {
			t.Fatalf("add account: %v", err)
		}
		// Expired, but its scanned plan carries the model: an overload, not a
		// 404, and nothing will recover.
		if err := db.UpdateQuota(acc.ID, store.Quota{
			Remaining: 10, Limit: 10,
			Entitlements: []map[string]any{{"name": "GLM-5.3", "limit": 10, "remaining": 10}},
		}); err != nil {
			t.Fatalf("set quota: %v", err)
		}
		db.SetStatusByID(acc.ID, "expired")
		return &Server{
			store: db, catalog: NewCatalog(nil),
			stickyAcc: map[string]int64{}, quota1005: map[string]int{}, chatOK: map[int64]time.Time{},
			cfg: config.Config{RequestHold: 0, AccountMaxTry: 1},
		}
	}

	cases := []struct {
		name   string
		path   string
		handle func(s *Server, w http.ResponseWriter, r *http.Request)
		body   string
		status int
	}{
		{
			name: "overload, openai", path: "/v1/chat/completions",
			handle: (*Server).handleChatCompletions,
			body:   `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`,
			status: 529,
		},
		{
			name: "overload, anthropic", path: "/v1/messages",
			handle: (*Server).handleMessages,
			body:   `{"model":"GLM-5.3","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			status: 529,
		},
		{
			// The plan is scanned and does not carry this model: 404, and the
			// client is not invited to retry.
			name: "model no plan offers", path: "/v1/chat/completions",
			handle: (*Server).handleChatCompletions,
			body:   `{"model":"NO-SUCH-MODEL","messages":[{"role":"user","content":"hi"}]}`,
			status: 404,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := expiredPool(t)
			before := monitor()

			rec := httptest.NewRecorder()
			c.handle(srv, rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body)))

			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, c.status, rec.Body.String())
			}
			after := monitor()
			if got := after.Requests - before.Requests; got != 1 {
				t.Fatalf("the refusal added %d requests to the log, want 1 — the client got a %d "+
					"and the monitor knows nothing about it", got, rec.Code)
			}
			if len(after.Recent) == 0 {
				t.Fatal("no recent entry recorded")
			}
			e := after.Recent[0]
			if e.Status != c.status {
				t.Errorf("logged status = %d, want the %d the client got", e.Status, c.status)
			}
			if e.OK {
				t.Error("a refusal was logged as ok")
			}
			if e.ErrNote == "" {
				t.Error("no reason recorded; an operator cannot tell an overload from a missing model")
			}
			// A refusal must never inflate the success rate. Before the fix the
			// denominator excluded it, so the figure stayed at whatever the
			// served requests were.
			if after.SuccessRate > before.SuccessRate {
				t.Errorf("SuccessRate rose from %.2f to %.2f after a refusal; a refused request "+
					"cannot be a success", before.SuccessRate, after.SuccessRate)
			}
		})
	}
}

// A malformed request is not the gateway's verdict about the pool, and no
// operator action follows from it: it must not pollute the ring that
// SuccessRate is computed from.
func TestMalformedRequestIsNotLoggedAsARejection(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := &Server{
		store: db, catalog: NewCatalog(nil),
		stickyAcc: map[string]int64{}, quota1005: map[string]int{}, chatOK: map[int64]time.Time{},
		cfg: config.Config{RequestHold: 0, AccountMaxTry: 1},
	}
	before := monitor()
	// A body that is not JSON at all: the OpenAI converter refuses it before
	// any account is considered.
	rec := httptest.NewRecorder()
	srv.handleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`not json at all`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := monitor().Requests - before.Requests; got != 0 {
		t.Errorf("a malformed body added %d to the log; it is the client's mistake, "+
			"not a pool verdict", got)
	}
}
