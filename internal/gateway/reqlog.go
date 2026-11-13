package gateway

// reqlog.go — request ring log + usage accounting
// go_eng usage tee): every chat request lands in a 500-entry ring with
// model/account/status/timing; SSE bodies are tee-parsed for the final
// usage (input/output/cache tokens) and accumulated per account.

import (
	"bytes"
	"encoding/json"
	"sync"
	"time"
)

type ReqEntry struct {
	TS      time.Time `json:"ts"`
	Model   string    `json:"model"`
	Account string    `json:"account"` // masked jwt
	OK      bool      `json:"ok"`
	Status  int       `json:"status"`
	Stream  bool      `json:"stream"`
	TFirst  int64     `json:"t_first_ms"`
	TTotal  int64     `json:"t_total_ms"`
	InTok   int       `json:"in_tok"`
	OutTok  int       `json:"out_tok"`
	Cache   int       `json:"cache_tok"`
	ErrNote string    `json:"err,omitempty"`
}

const reqRingSize = 500

type reqLog struct {
	mu    sync.Mutex
	ring  []ReqEntry
	idx   int
	stats struct {
		total    int64
		ok       int64
		inTok    int64
		outTok   int64
		cacheTok int64
	}
	started time.Time
}

var rlog = &reqLog{ring: make([]ReqEntry, reqRingSize), started: time.Now()}

func (l *reqLog) Add(e ReqEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring[l.idx] = e
	l.idx = (l.idx + 1) % len(l.ring)
	l.stats.total++
	if e.OK {
		l.stats.ok++
	}
	l.stats.inTok += int64(e.InTok)
	l.stats.outTok += int64(e.OutTok)
	l.stats.cacheTok += int64(e.Cache)
}

// LogOK records a finished request (forward.go hot path). status and ok are
// what the gateway wrote, not what the Origin returned: a 2xx the gateway could
// not render goes out as 502, and a stream that fails after its first chunk
// keeps the 200 already sent, so the status alone no longer says whether the
// client got a reply.
func LogOK(mdl, acc string, status int, ok, stream bool, tFirst, tTotal time.Duration, usg Usage) {
	rlog.Add(ReqEntry{
		TS: time.Now(), Model: mdl, Account: acc, OK: ok,
		Status: status, Stream: stream,
		TFirst: tFirst.Milliseconds(), TTotal: tTotal.Milliseconds(),
		InTok: usg.In, OutTok: usg.Out, Cache: usg.CacheRead + usg.CacheCreate,
	})
}

// LogReject records a request the gateway answered without ever relaying a
// reply: an overload, a model no plan offers, a captcha outage. LogOK covers
// only the path that reached an Origin answer, so before this the whole class
// of refusals was absent from the ring — /api/monitor reported zero requests
// for a gateway refusing every one of them, and SuccessRate read as a clean
// record for a pool that was answering nothing.
//
// A refusal counts towards the total but never towards ok: the gateway did not
// serve the request. That means SuccessRate drops on a deployment that was
// refusing requests all along — the figure was wrong before, and this is what
// it looks like once refusals are counted.
//
// The gateway's own verdicts are logged; a malformed request (405 on a wrong
// method, 400 on a missing model) is not, because it says nothing about the
// pool and no operator action follows from it.
func LogReject(mdl string, status int, stream bool, note string) {
	rlog.Add(ReqEntry{
		TS: time.Now(), Model: mdl, OK: false,
		Status: status, Stream: stream,
		ErrNote: note,
	})
}

// Usage — token triple extracted from the SSE stream.
type Usage struct {
	In          int
	Out         int
	CacheRead   int
	CacheCreate int
}

// usageScanner parses Anthropic SSE chunks for usage fields. The origin splits
// the token triple across events — message_start carries input/output only,
// message_delta carries the cumulative totals including the cache counters — so
// each counter is taken from whichever event supplies it.
type usageScanner struct {
	buf   bytes.Buffer
	found Usage
}

func (u *usageScanner) Write(p []byte) (int, error) {
	u.buf.Write(p)
	for {
		line, err := u.buf.ReadBytes('\n')
		if err != nil {
			// partial line — put it back for the next write
			u.buf.Write(line)
			break
		}
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(trimmed[5:])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Usage *struct {
				InputTokens              *int `json:"input_tokens"`
				OutputTokens             *int `json:"output_tokens"`
				CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			Message struct {
				Usage *struct {
					InputTokens              *int `json:"input_tokens"`
					OutputTokens             *int `json:"output_tokens"`
					CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			continue
		}
		// Counters arrive as cumulative snapshots, and different events carry
		// different subsets: message_start has input/output only, message_delta
		// adds the cache counters. Assign whichever are present (they are the
		// latest totals) and leave the rest alone, so an event that omits a
		// field cannot zero out a value an earlier event reported.
		apply := func(in, out, cacheRead, cacheCreate *int) {
			if in != nil {
				u.found.In = *in
			}
			if out != nil {
				u.found.Out = *out
			}
			if cacheRead != nil {
				u.found.CacheRead = *cacheRead
			}
			if cacheCreate != nil {
				u.found.CacheCreate = *cacheCreate
			}
		}
		if ev.Usage != nil { // top-level usage: the cumulative totals
			apply(ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Usage.CacheReadInputTokens, ev.Usage.CacheCreationInputTokens)
		}
		if ev.Message.Usage != nil { // usage nested in the message object
			apply(ev.Message.Usage.InputTokens, ev.Message.Usage.OutputTokens, ev.Message.Usage.CacheReadInputTokens, ev.Message.Usage.CacheCreationInputTokens)
		}
	}
	return len(p), nil
}

// Usage returns the scanned token triple. Falls back to parsing the
// buffered body as a plain JSON response (non-stream path: usage sits in
// the message object, not SSE data lines).
func (u *usageScanner) Usage() Usage {
	if u.found.In == 0 && u.found.Out == 0 {
		var resp struct {
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(u.buf.Bytes(), &resp) == nil && resp.Usage != nil {
			return Usage{
				In: resp.Usage.InputTokens, Out: resp.Usage.OutputTokens,
				CacheRead:   resp.Usage.CacheReadInputTokens,
				CacheCreate: resp.Usage.CacheCreationInputTokens,
			}
		}
	}
	return u.found
}

// monitorSnapshot powers /api/monitor.
type monitorSnapshot struct {
	Uptime      string     `json:"uptime"`
	Requests    int64      `json:"requests"`
	SuccessRate float64    `json:"success_rate"`
	QPS         float64    `json:"qps"`
	InTok       int64      `json:"in_tok"`
	OutTok      int64      `json:"out_tok"`
	CacheTok    int64      `json:"cache_tok"`
	Recent      []ReqEntry `json:"recent"`
}

func monitor() monitorSnapshot {
	l := rlog
	l.mu.Lock()
	defer l.mu.Unlock()
	up := time.Since(l.started).Seconds()
	sr := 0.0
	if l.stats.total > 0 {
		sr = float64(l.stats.ok) / float64(l.stats.total)
	}
	qps := 0.0
	if up > 0 {
		qps = float64(l.stats.total) / up
	}
	recent := make([]ReqEntry, 0, reqRingSize)
	for i := 0; i < reqRingSize; i++ {
		j := (l.idx + reqRingSize - 1 - i + reqRingSize) % reqRingSize // newest first
		if !l.ring[j].TS.IsZero() {
			recent = append(recent, l.ring[j])
		}
	}
	return monitorSnapshot{
		Uptime:   time.Since(l.started).Round(time.Second).String(),
		Requests: l.stats.total, SuccessRate: sr, QPS: qps,
		InTok: l.stats.inTok, OutTok: l.stats.outTok, CacheTok: l.stats.cacheTok,
		Recent: recent,
	}
}
