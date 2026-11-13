package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"zcode2api/internal/upstream"
	"zcode2api/internal/util"
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 50<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The OpenAI-only request options have to be read from the client's body:
	// the Anthropic wire format has no place for them, so the conversion below
	// drops them.
	includeUsage := ParseIncludeUsage(raw)
	switch ClassifyResponseFormat(raw) {
	case rfUnsupported:
		// The client set strict: true — an explicit contract for schema
		// conformance we cannot provide. Refuse now rather than return prose it
		// will fail to parse with no clue why. See docs/adr/0001.
		writeOpenAIError(w, http.StatusBadRequest,
			"response_format.json_schema.strict is not supported by this gateway: "+
				"the origin does not enforce JSON schemas, so the schema would be silently ignored")
		return
	case rfDropped:
		log.Printf("[openai] response_format dropped: the origin does not enforce JSON schemas")
	}
	anth, err := OpenAIToAnthropic(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.routeAnthropic(w, r, anth, true, includeUsage)
}

// responseFormatKind classifies what a request's response_format asks of us.
type responseFormatKind int

const (
	// rfNone: no response_format, or an explicit "text" — nothing to do.
	rfNone responseFormatKind = iota
	// rfDropped: the client asked for JSON but not for a guarantee, and the
	// origin cannot honour a schema. Serve normally; the field does not reach
	// the origin, so log that it was dropped.
	rfDropped
	// rfUnsupported: the client set strict: true, which is an explicit contract
	// for schema conformance. We cannot provide it, so refuse rather than hand
	// back prose the client will fail to parse with no clue why.
	rfUnsupported
)

// ClassifyResponseFormat reads the OpenAI-only response_format option from the
// client's body — it has no Anthropic equivalent, so the conversion downstream
// drops it and it has to be inspected first.
//
// `strict` lives at response_format.json_schema.strict, is optional, and
// defaults to false, so a request that omits it is ordinary best-effort and
// must keep working. Anything we cannot parse is treated as non-strict: a
// malformed field we were going to drop anyway is not worth rejecting over.
func ClassifyResponseFormat(raw []byte) responseFormatKind {
	// Two passes. The outer one only asks "is response_format there at all", so
	// a nested shape we cannot read still counts as a request that asked for
	// something. The inner one reads the fields, and anything it cannot parse
	// falls through to dropped — never to silence.
	var outer struct {
		ResponseFormat json.RawMessage `json:"response_format"`
	}
	if json.Unmarshal(raw, &outer) != nil {
		// The body does not parse at all; the conversion rejects it a moment
		// from now, so there is nothing to warn about here.
		return rfNone
	}
	if len(outer.ResponseFormat) == 0 || string(outer.ResponseFormat) == "null" {
		return rfNone
	}

	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Strict *bool `json:"strict"`
		} `json:"json_schema"`
	}
	if json.Unmarshal(outer.ResponseFormat, &rf) != nil {
		return rfDropped
	}
	if rf.Type == "text" {
		return rfNone
	}
	if rf.Type == "json_schema" && rf.JSONSchema != nil &&
		rf.JSONSchema.Strict != nil && *rf.JSONSchema.Strict {
		return rfUnsupported
	}
	// Everything else carries a response_format we are going to discard —
	// json_object, json_schema, a type we do not recognise, or a shape that did
	// not parse far enough to tell. Warn either way: the client asked for
	// something and is not getting it, and staying quiet is the failure mode
	// this whole path exists to prevent.
	return rfDropped
}

func OpenAIToAnthropic(raw []byte) ([]byte, error) {
	var req struct {
		Model            string          `json:"model"`
		Stream           bool            `json:"stream"`
		MaxTokens        int             `json:"max_tokens"`
		MaxCompletionTok int             `json:"max_completion_tokens"`
		Stop             []string        `json:"stop"`
		ToolChoice       json.RawMessage `json:"tool_choice"`
		Messages         []oaMessage     `json:"messages"`
		Tools            []oaTool        `json:"tools"`
		Thinking         json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = req.MaxCompletionTok // OpenAI's newer field
	}
	if maxTok > 0 {
		out["max_tokens"] = maxTok
	} else {
		out["max_tokens"] = 4096 // OpenAI-compat default
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}
	if len(req.Thinking) > 0 && string(req.Thinking) != "null" {
		var t any
		_ = json.Unmarshal(req.Thinking, &t)
		out["thinking"] = t
	}
	var system []any
	var msgs []any
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, map[string]any{"type": "text", "text": m.Text()})
			continue
		}
		msgs = append(msgs, openaiMsgToAnth(m))
	}
	if len(system) > 0 {
		out["system"] = system
	}
	out["messages"] = msgs
	if len(req.Tools) > 0 {
		var tools []any
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": schema,
			})
		}
		// tool_choice: none drops tools entirely; required/named map through
		choice := "auto"
		var tcName string
		if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
			var tc any
			if json.Unmarshal(req.ToolChoice, &tc) == nil {
				switch v := tc.(type) {
				case string:
					if v == "none" {
						tools = nil
					} else if v == "required" {
						choice = "any"
					}
				case map[string]any:
					if fn, ok := v["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok {
							choice = "tool"
							tcName = n
						}
					}
				}
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
			if choice == "tool" {
				out["tool_choice"] = map[string]any{"type": "tool", "name": tcName}
			} else {
				out["tool_choice"] = map[string]any{"type": choice}
			}
		}
	}
	return json.Marshal(out)
}

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

func (m oaMessage) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	if m.Content[0] == '"' {
		var s string
		_ = json.Unmarshal(m.Content, &s)
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(m.Content)
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func openaiMsgToAnth(m oaMessage) map[string]any {
	if m.Role == "tool" {
		return map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Text(),
			}},
		}
	}
	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		var blocks []any
		if t := m.Text(); t != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": t})
		}
		for _, tc := range m.ToolCalls {
			var input any
			if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
				input = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
		return map[string]any{"role": "assistant", "content": blocks}
	}
	// multimodal content: text parts + image_url data-URLs → image blocks
	if blocks := m.contentBlocks(); blocks != nil {
		return map[string]any{"role": m.Role, "content": blocks}
	}
	return map[string]any{"role": m.Role, "content": m.Text()}
}

// contentBlocks converts OpenAI multimodal parts to Anthropic blocks.
// Data-URL images become base64 image blocks; remote URLs are skipped
// Nil = plain string content.
func (m oaMessage) contentBlocks() []any {
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	var blocks []any
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			if media, data, ok := strings.Cut(p.ImageURL.URL, ","); ok &&
				strings.HasPrefix(media, "data:") {
				fmt_ := strings.TrimPrefix(media, "data:")
				if i := strings.Index(fmt_, ";"); i >= 0 {
					fmt_ = fmt_[:i]
				}
				blocks = append(blocks, map[string]any{
					"type": "image", "source": map[string]any{
						"type": "base64", "media_type": fmt_, "data": data,
					},
				})
			}
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return blocks
}

// newOAChunk builds a stream chunk envelope. The same five keys appear on every
// chunk; only the choices and the optional usage differ.
func newOAChunk(id, model string, choices []any, usage map[string]any) map[string]any {
	if choices == nil {
		choices = []any{}
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": choices,
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return chunk
}

// writeSSEChunk marshals one chunk and writes it as an SSE frame, flushing so
// the client sees it immediately.
func writeSSEChunk(w http.ResponseWriter, fl http.Flusher, chunk map[string]any) {
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

func (s *Server) writeOpenAIFromOrigin(w http.ResponseWriter, resp *http.Response, head []byte, rest io.ReadCloser, outModel string, stream bool, scn *usageScanner, includeUsage bool) {
	defer resp.Body.Close()
	id := "chatcmpl-" + util.GenerateUUID()
	// rest (the wrapped resp.Body) already starts with head — head feeds
	// detection only, never the byte stream
	//
	// The status decides the shape of the answer, before the body is read as a
	// reply. Testing parseability instead is what turned an Origin error into a
	// fabricated chat.completion carrying an error status.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(rest, 8<<20))
		writeOpenAIFromOriginError(w, resp.StatusCode, raw)
		return
	}
	if !upstream.IsSSE(head) {
		// non-stream upstream: a complete Anthropic message JSON — convert it
		writeOpenAIFromJSON(w, resp.StatusCode, rest, id, outModel, scn)
		return
	}
	src := rest
	if !stream {
		text, tools, err := collectAnthropic(src)
		if err != nil {
			// Nothing has been written yet, so this is an ordinary 502. The
			// message is the Origin's own when it reported an error event; a
			// stream that just stopped, or that the scanner could not read,
			// gets the fixed one — scanner internals are not for the client.
			log.Printf("[openai] Origin stream could not be rendered as a reply: %v", err)
			writeOpenAIError(w, http.StatusBadGateway, originStreamMessage(err))
			return
		}
		msg := map[string]any{"role": "assistant", "content": text}
		finish := "stop"
		if len(tools) > 0 {
			msg["tool_calls"] = tools
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(withUsage(map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   outModel,
			"choices": []any{map[string]any{
				"index": 0, "message": msg, "finish_reason": finish,
			}},
		}, scn.Usage()))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	st := &oaStreamState{toolIdx: map[int]int{}}
	// Headers are only Set above; the response commits on the first chunk
	// written. Until then a failure can still be a real 502, which SDKs branch
	// on before they parse a single SSE line. After it, the only channel left
	// is one error frame inside the stream.
	wrote := false
	err := scanAnthropicSSE(src, func(ev string, data map[string]any) {
		if st.failed {
			return // nothing after an Origin error is part of the reply
		}
		if chunk := anthEventToOA(ev, data, id, outModel, st); chunk != nil {
			writeSSEChunk(w, fl, chunk)
			wrote = true
		}
	})
	if err != nil || !st.complete() {
		// An Origin error event, a stream that ended before a finish trigger,
		// or a body the scanner could not read. Each used to end in [DONE] —
		// or in silence — and read as a clean, possibly empty, answer.
		msg := "origin returned a malformed response"
		if st.failed {
			msg = st.errMsg
		}
		if err != nil {
			log.Printf("[openai] Origin stream could not be read: %v", err)
		} else if !st.failed {
			log.Printf("[openai] Origin stream ended before the message finished")
		}
		if !wrote {
			writeOpenAIError(w, http.StatusBadGateway, msg)
			return
		}
		writeSSEChunk(w, fl, oaErrorFrame(msg))
		return
	}
	// The scanner has now seen the whole body, message_delta included, so the
	// token triple is complete. A client that asked for it gets one terminal
	// chunk with an empty choices array — the shape OpenAI uses — and then the
	// [DONE] sentinel. Clients that did not ask see exactly what they saw
	// before.
	if includeUsage {
		if usage := oaUsage(scn.Usage()); usage != nil {
			writeSSEChunk(w, fl, newOAChunk(id, outModel, nil, usage))
		}
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// oaUsage renders a scanned Anthropic usage triple as the OpenAI `usage`
// object. Anthropic reports input_tokens with the cached prompt tokens
// excluded, while OpenAI's prompt_tokens means every token in the prompt, so
// the cache counters are summed in — once, not again in total_tokens.
//
// completion_tokens_details is deliberately absent: the origin reports no
// reasoning-token count, and a zero there would read as "reasoning measured,
// none used" rather than "not measured".
//
// Returns nil when nothing was measured, for the same reason: an all-zero
// object claims a measurement of zero. The caller omits the key instead.
func oaUsage(u Usage) map[string]any {
	if u.In == 0 && u.Out == 0 && u.CacheRead == 0 && u.CacheCreate == 0 {
		return nil
	}
	prompt := u.In + u.CacheRead + u.CacheCreate
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": u.Out,
		"total_tokens":      prompt + u.Out,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.CacheRead,
		},
	}
}

// withUsage attaches the decoded usage object to an encoded completion, or
// leaves the key out entirely when there was nothing to report. Callers build
// their envelope as a map; this keeps the omit-empty rule in one place so the
// two non-stream branches cannot disagree about it.
func withUsage(body map[string]any, u Usage) map[string]any {
	if usage := oaUsage(u); usage != nil {
		body["usage"] = usage
	}
	return body
}

// writeOpenAIFromOriginError renders an Origin error reply as an OpenAI error
// envelope. The Origin answers errors in Anthropic shape —
// {"type":"error","error":{"type":"...","code":"1213","message":"..."}} — so the
// code and message are lifted out of that nest when present, and the status is
// whatever the Origin chose.
//
// This runs *before* any attempt to read the body as a completion: deciding on
// parseability instead is what produced a fabricated chat.completion attached to
// an HTTP 400.
func writeOpenAIFromOriginError(w http.ResponseWriter, status int, raw []byte) {
	var doc struct {
		Error *struct {
			// The Origin sends code as a string ("1213"), but other sources
			// send a number. Accept either rather than silently losing it.
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &doc)

	msg := "the Origin rejected the request"
	var code *string
	if doc.Error != nil {
		if doc.Error.Message != "" {
			msg = doc.Error.Message
		}
		if c := rawScalarString(doc.Error.Code); c != "" {
			code = &c
		}
	}
	if doc.Error == nil && len(bytes.TrimSpace(raw)) > 0 {
		// Not the error shape we expected. The body could be an HTML page from
		// something in between, or an internal trace — none of which belongs in
		// a client-facing message. Keep it in the server log for diagnosis and
		// tell the client something stable. The clip is deliberate here and
		// absent for a malformed 2xx body (writeOpenAIFromJSON): a refusal's
		// body is the Origin's own account of why, and an operator needs to see
		// whether it was a WAF page or a proxy; a garbage success body has
		// nothing in it to read.
		log.Printf("[openai] Origin returned %d with an unrecognised error body: %s",
			status, clip(string(raw), 200))
	}
	writeOpenAIErrorDetail(w, status, msg, code)
}

// rawScalarString renders a JSON string or number as a plain string, and
// anything else as "". Origin error codes arrive as either type.
func rawScalarString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// writeOpenAIFromJSON converts a complete Anthropic message document (the
// origin's non-stream reply) into an OpenAI chat.completion.
func writeOpenAIFromJSON(w http.ResponseWriter, status int, body io.Reader, id, outModel string, scn *usageScanner) {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "read origin response: "+err.Error())
		return
	}
	var doc struct {
		Type       string          `json:"type"`
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		// A success status with a body that is not a message document: a page
		// from something in between, a truncated reply. Echoing it under the
		// Origin's 200 handed clients an error envelope labelled as success,
		// with whatever the body held pasted into it. The parse error names the
		// offset, which is enough to diagnose without logging the body itself.
		log.Printf("[openai] Origin returned %d with a body that is not a message document (%d bytes): %v", status, len(raw), err)
		writeOpenAIError(w, http.StatusBadGateway, "origin returned a malformed response")
		return
	}
	// Valid JSON is not yet a reply. `null`, `{}` or an unrelated object decode
	// into an empty document without error, and used to come out the other side
	// as a completion with empty content and finish_reason "stop" — a turn the
	// client believes the model finished. A message document carries its blocks
	// in a content array; that is the floor, and the one field every real reply
	// has, tool-use ones included. The type is checked only when present: the
	// Origin's error document says "error" there, and some replies omit it.
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if !startsWithByte(doc.Content, '[') || (doc.Type != "" && doc.Type != "message") || json.Unmarshal(doc.Content, &blocks) != nil {
		log.Printf("[openai] Origin returned %d with JSON that is not a message document (%d bytes)", status, len(raw))
		writeOpenAIError(w, http.StatusBadGateway, "origin returned a malformed response")
		return
	}
	text := strings.Builder{}
	var tools []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			tools = append(tools, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	finish := oaFinishReason(doc.StopReason)
	if len(tools) > 0 {
		msg["tool_calls"] = tools
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(withUsage(map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   outModel,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
	}, scn.Usage()))
}

// startsWithByte reports whether the first non-space byte of raw is c.
func startsWithByte(raw []byte, c byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == c
}

// errOriginStream is why an Anthropic event stream could not be rendered as a
// reply: the Origin reported an error, or the stream ended before a finish
// trigger. Its message is safe to show the client.
type errOriginStream struct{ msg string }

func (e *errOriginStream) Error() string { return e.msg }

// originStreamMessage is the client-facing message for a collection failure:
// the Origin's own words when it reported an error, the fixed one otherwise,
// so scanner internals never reach the client.
func originStreamMessage(err error) string {
	var oe *errOriginStream
	if errors.As(err, &oe) {
		return oe.msg
	}
	return "origin returned a malformed response"
}

// collectAnthropic gathers a whole Anthropic event stream into the text and
// tool calls of one reply. It is a reply only if the stream reached a finish
// trigger without an error event; otherwise an empty collection is not an empty
// answer, and the error says why.
func collectAnthropic(r io.Reader) (string, []any, error) {
	var text strings.Builder
	var tools []any
	var curTool map[string]any
	var args strings.Builder
	var lc streamLifecycle
	err := scanAnthropicSSE(r, func(ev string, data map[string]any) {
		if lc.failed {
			return
		}
		switch ev {
		case "message_delta":
			md, _ := data["delta"].(map[string]any)
			if upstream.Str(md["stop_reason"]) != "" {
				lc.finish()
			}
		case "message_stop":
			lc.finish()
		case "error":
			lc.fail(data)
		case "content_block_start":
			cb, _ := data["content_block"].(map[string]any)
			if upstream.Str(cb["type"]) == "tool_use" {
				curTool = map[string]any{
					"id": upstream.Str(cb["id"]), "type": "function",
					"function": map[string]any{"name": upstream.Str(cb["name"]), "arguments": ""},
				}
				args.Reset()
			}
		case "content_block_delta":
			d, _ := data["delta"].(map[string]any)
			switch upstream.Str(d["type"]) {
			case "text_delta":
				text.WriteString(upstream.Str(d["text"]))
			case "input_json_delta":
				args.WriteString(upstream.Str(d["partial_json"]))
			}
		case "content_block_stop":
			if curTool != nil {
				fn, _ := curTool["function"].(map[string]any)
				fn["arguments"] = args.String()
				tools = append(tools, curTool)
				curTool = nil
			}
		}
	})
	if err != nil {
		return "", nil, err
	}
	if lc.failed {
		return "", nil, &errOriginStream{msg: lc.errMsg}
	}
	if !lc.finished {
		return "", nil, &errOriginStream{msg: "origin returned a malformed response"}
	}
	return text.String(), tools, nil
}

func scanAnthropicSSE(r io.Reader, fn func(event string, data map[string]any)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var ev string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var data map[string]any
		if json.Unmarshal([]byte(payload), &data) != nil {
			continue
		}
		if t := upstream.Str(data["type"]); t != "" {
			ev = t
		}
		fn(ev, data)
	}
	return sc.Err()
}

// streamLifecycle is what both readers of an Anthropic event stream need to
// know about its shape: whether a finish trigger (message_delta with a
// stop_reason, or message_stop) has been seen, and whether the Origin reported
// an error on the way. A stream is a reply only when the first happened and the
// second did not; anything else ending in [DONE] or a completion is a clean-
// looking lie.
type streamLifecycle struct {
	finished bool
	failed   bool
	errMsg   string // the Origin's error message, or a fixed fallback
}

// finish records a finish trigger and reports whether it was the first. Only
// the first wins: the Origin can send several message_delta events, and a
// repeat must not emit a second finish_reason or let message_stop overwrite
// "tool_calls" with "stop".
func (l *streamLifecycle) finish() bool {
	if l.finished {
		return false
	}
	l.finished = true
	return true
}

// fail records an Origin error event.
func (l *streamLifecycle) fail(data map[string]any) {
	e, _ := data["error"].(map[string]any)
	l.errMsg = upstream.Str(e["message"])
	if l.errMsg == "" {
		l.errMsg = "the Origin failed mid-stream"
	}
	l.failed = true
}

// complete reports whether the stream can be called a reply.
func (l *streamLifecycle) complete() bool { return l.finished && !l.failed }

// oaStreamState tracks one converted stream. OpenAI numbers tool calls
// 0,1,2… among tool calls only, while Anthropic's index counts every content
// block (thinking and text included), so the two must be mapped.
type oaStreamState struct {
	streamLifecycle
	toolIdx map[int]int
}

// oaErrorFrame is the one SSE frame a stream can carry once it has started and
// the Origin fails: the OpenAI error envelope, which clients branch on.
func oaErrorFrame(msg string) map[string]any {
	return map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "api_error",
		"param":   nil,
		"code":    nil,
	}}
}

func oaFinishReason(stopReason string) string {
	switch stopReason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	}
	return "stop"
}

func anthEventToOA(ev string, data map[string]any, id, model string, st *oaStreamState) map[string]any {
	delta := map[string]any{}
	var finish *string
	switch ev {
	case "content_block_delta":
		d, _ := data["delta"].(map[string]any)
		switch upstream.Str(d["type"]) {
		case "text_delta":
			delta["content"] = upstream.Str(d["text"])
		case "thinking_delta":
			delta["reasoning_content"] = upstream.Str(d["thinking"])
		case "input_json_delta":
			// A delta whose content_block_start went missing (a dropped or
			// truncated SSE line) must not silently discard the arguments: the
			// tool call was already announced with `arguments: ""`, so dropping
			// the payload leaves the client parsing an empty string. Fall back to
			// the block index itself, then allocate a slot for it so any further
			// deltas for the same block agree.
			upIdx := upstream.AsInt(data["index"])
			idx, ok := st.toolIdx[upIdx]
			if !ok {
				idx = len(st.toolIdx)
				st.toolIdx[upIdx] = idx
				log.Printf("[openai] tool delta for unseen content block %d — allocating tool index %d", upIdx, idx)
			}
			delta["tool_calls"] = []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": upstream.Str(d["partial_json"])},
			}}
		default:
			return nil
		}
	case "content_block_start":
		cb, _ := data["content_block"].(map[string]any)
		if upstream.Str(cb["type"]) != "tool_use" {
			return nil
		}
		idx := len(st.toolIdx)
		st.toolIdx[upstream.AsInt(data["index"])] = idx
		delta["tool_calls"] = []any{map[string]any{
			"index": idx, "id": upstream.Str(cb["id"]), "type": "function",
			"function": map[string]any{"name": upstream.Str(cb["name"]), "arguments": ""},
		}}
	case "message_delta":
		md, _ := data["delta"].(map[string]any)
		if sr := upstream.Str(md["stop_reason"]); sr != "" {
			if !st.finish() {
				return nil
			}
			fr := oaFinishReason(sr)
			finish = &fr
		}
	case "message_stop":
		if !st.finish() {
			return nil
		}
		fr := "stop"
		finish = &fr
	case "error":
		// The Origin can fail mid-stream. Recorded here; the writer decides
		// whether that becomes a 502 or an in-stream error frame, depending on
		// whether anything has reached the client yet.
		st.fail(data)
		return nil
	default:
		return nil
	}
	// The OpenAI schema marks finish_reason required (nullable) on every stream
	// choice, so it is always present: null while the turn is still running, the
	// real reason on the terminal chunk. Omitting it on content chunks is what
	// strict clients reject.
	ch := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		ch["finish_reason"] = *finish
	}
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": model, "choices": []any{ch},
	}
}

// oaErrorType maps an HTTP status to the error.type OpenAI clients branch on.
// A single string for every status makes that branch unusable, and 401 vs 400
// matters: one says "your key is wrong", the other "your request is wrong".
func oaErrorType(code int) string {
	switch code {
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	}
	if code >= 400 && code < 500 {
		return "invalid_request_error"
	}
	return "api_error"
}

func writeOpenAIError(w http.ResponseWriter, code int, msg string) {
	writeOpenAIErrorDetail(w, code, msg, nil)
}

// writeOpenAIErrorDetail is writeOpenAIError with the Origin's own error code
// carried through, so a client can tell "the origin rejected the request with
// 1213" from a generic gateway string.
//
// param and code are always present: the bundled spec marks all four members of
// Error required, and a missing key fails the ErrorResponse schema — null says
// "unknown", absent says "malformed".
func writeOpenAIErrorDetail(w http.ResponseWriter, status int, msg string, originCode *string) {
	writeOpenAIErrorFull(w, status, msg, nil, originCode)
}

// writeOpenAIErrorFull is the full envelope: every member the spec marks
// required, with param and code addressable so an error can point at the field
// that caused it and carry a machine-readable code.
func writeOpenAIErrorFull(w http.ResponseWriter, status int, msg string, param, code *string) {
	var paramV, codeV any
	if param != nil {
		paramV = *param
	}
	if code != nil {
		codeV = *code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    oaErrorType(status),
			"param":   paramV,
			"code":    codeV,
		},
	})
}

// modelNameLimit bounds the model name echoed into an error message. The name
// comes from the client, so an unbounded echo lets a careless or hostile caller
// inflate the error body.
const modelNameLimit = 100

// writeOpenAIModelNotFound answers a model the gateway cannot serve. It is a
// missing resource, not a malformed request: 404 with the code OpenAI SDKs
// branch on. No Retry-After — nothing will change, so inviting a retry would be
// a lie.
func writeOpenAIModelNotFound(w http.ResponseWriter, model string) {
	if len(model) > modelNameLimit {
		model = model[:modelNameLimit] + "…"
	}
	param, code := "model", "model_not_found"
	// The message deliberately says nothing about pools or accounts: that is
	// internal structure the caller has no business inferring.
	//
	// The type stays invalid_request_error rather than the not_found_error the
	// status mapper would pick: OpenAI does not constrain type, and code is the
	// field that distinguishes this error. Inventing a type clients never had to
	// handle is a change with no upside.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": "model " + model + " is not available",
			"type":    "invalid_request_error",
			"param":   param,
			"code":    code,
		},
	})
}

func writeAnthropicError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error", "error": map[string]any{"type": typ, "message": msg},
	})
}
