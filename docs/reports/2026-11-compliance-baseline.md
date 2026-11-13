# Compliance baseline — `chat_completions`, 2026-11-13

A verified snapshot of what this gateway does and does not satisfy on the OpenAI
chat-completions surface. Keep it as a reference point: when compliance work
resumes, measure against this rather than re-deriving the current state.

## What was measured

- **Gateway revision:** `5624a88` (+ the `error.type` fix that followed it)
- **Verifier:** `llm-comply` 0.6.0, built from `Oaklight/llm-comply` master
- **Spec:** bundled `openai_chat.json` v2.3.0 (`chat_completions`)
- **Scope:** `chat_completions` only. The Anthropic `messages` format was not
  run — it fails for reasons outside this repository's control.

Reproduce:

```bash
GW=$(grep -E '^AUTH_TOKEN=' .env | cut -d= -f2)
docker exec llm-comply llm-comply --format chat_completions \
  -u http://172.17.0.1:3101/v1 -k "$GW" -m GLM-5.3
```

## Result

Four runs (two models × two passes), stable:

```
GLM-5.3        Results: 9/13 passed, 2 failed, 7 with warnings, 2 skipped
GLM-5.3-Flash  Results: 9/13 passed, 2 failed, 7 with warnings, 2 skipped
```

Model makes no difference to any outcome — both go through the same path.

| Test | Result |
|---|---|
| `chat-basic-response` | passed (fixed in this work) |
| `chat-streaming-usage` | passed (fixed in this work) |
| `chat-streaming-response` | passed, warning cleared in this work |
| `chat-structured-output` | **failed, deliberately** — origin does not enforce schemas; `strict: true` is refused with 400. See ADR-0001 |
| `chat-error-handling` | **failed, deliberately** — default model is a recorded deviation. See `docs/deviations.md` |
| 6 others | passed |
| `chat-reasoning-summary`, `chat-audio-output` | skipped — model-name heuristics |

## What was fixed to get here

- Cache counters were read from the wrong event, so `/api/monitor` reported
  `cache_tok: 0` for every streamed request.
- `usage` was absent from non-stream completions.
- `stream_options.include_usage` was ignored, so the terminal usage chunk never
  arrived.
- `finish_reason` was omitted on stream chunks.
- `response_format` was dropped without a word; `strict: true` is now refused.

## Known gaps and unverified branches

**Fake completion on an upstream 4xx — fixed.** An Origin error whose body was
valid JSON used to be rendered as a completion with the error status attached.
The status now decides the response shape before the body is read as a reply, and
a mid-stream error event reaches the client as an error frame rather than an
empty stream closed by `[DONE]`. The measurement below predates that fix and is
unchanged by it — the failing-test set is the same.

**Not verified end to end**, only by unit test:

- the guard that stops a repeated `message_delta` from emitting a second
  `finish_reason` — the Origin always sends exactly one, so it cannot be
  reproduced with a live request;
- the branch that omits `usage` when nothing was measured — no successful
  response could be produced without usage in it;
- `CacheCreate` non-zero on a live request — only `CacheRead` was observed.

Also note: 90 requests with no 5xx is a small sample, not evidence of absence.

## Warning set

Seven non-stream tests warn about `refusal`, `annotations`, `logprobs` and
`usage.completion_tokens_details`. All four are strict-OpenAI fields with no GLM
equivalent, or deliberate omissions. None is a defect; see `docs/deviations.md`
for the reasoning on `completion_tokens_details`.
