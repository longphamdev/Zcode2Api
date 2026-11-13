# No structured-output emulation at the gateway

Status: accepted

A client can ask for a JSON Schema through OpenAI's `response_format`, but the
Origin does not enforce schemas, so the gateway does not pretend otherwise. We
forward nothing for it and rewrite nothing in the reply.

## Context

`response_format` has no equivalent in the Anthropic wire format the Origin
speaks. The nearest field, `output_config.format`, is accepted and then ignored:
six runs with a demanding schema — `enum`, `required`,
`additionalProperties: false` — returned prose every time, while a control
prompt without any schema returned clean JSON on demand. The gap is in the
Origin, not in this gateway.

## Decision

Do not splice the schema into the client's system prompt, and do not strip
markdown fences from replies, in order to make a client believe a schema was
enforced.

## Considered options

- **Prompt-level coercion plus fence stripping.** Rejected. It rewrites the
  client's own prompt, so it can affect instructions that have nothing to do
  with the schema; it cannot prove conformance — a fence-stripped reply may
  still violate the schema; and it only "passes" when the model happens to
  cooperate, which is not a guarantee a client can rely on.
- **Forward `response_format` and hope.** Rejected. The Origin ignores it, so
  nothing about the response changes while the client believes it was honoured.
  Silent and worse than an honest refusal.

## Consequences

A client asking for structured output gets no guarantee. Requests carrying
`response_format` are still accepted and the field dropped; the strict case is
handled separately — see the ticket on rejecting `strict: true`.

## Revisit when

The Origin starts enforcing the schema — checkable with the same control test
used above — or the gateway gains a trustworthy enforcement mechanism, meaning
it validates the reply against the schema and returns an error rather than
passing bad content through.

---

## Known deviations

Deliberate departures from the OpenAI specification. A reader comparing this
gateway against the spec will find these surprising if not recorded, and might
"fix" them.

**A request without `model` is served, using a default.** The OpenAI schema
marks `model` required; this gateway fills in `GLM-5.3` instead of rejecting.
The rationale is that this is a convenience proxy, not the OpenAI API: requiring
`model` on every request only makes clients harder to write. The consequence is
that a malformed client never learns its request is malformed — it gets a
perfectly good completion attributed to a model it never asked for.

Reversing this is a real cost rather than a one-line change: clients grow to
rely on the default, so tightening it later breaks them. The async path
(`/async/v1/messages`) bypasses the default today and would have to be brought
into line in the same change, or the two paths would disagree about what a
model-less request means.

**`response_format` is accepted and dropped.** Requests carrying
`json_object`, or `json_schema` without `strict: true`, succeed and the field is
discarded — best effort from the model's own reading of the prompt, with no
schema constraint. `strict: true` is the case where a client is explicitly
contracting for a guarantee, and is handled separately.

**`completion_tokens_details` is never emitted.** There is no trustworthy
source for `reasoning_tokens` in what the Origin reports. A zero would read as
"measured, none used" rather than "not measured", so the object is omitted
instead. The same rule governs `usage` itself: when nothing was measured, the
key is absent rather than all zeros.
