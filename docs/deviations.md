# Deviations from the OpenAI specification

Deliberate departures, gathered in one place. A reader comparing this gateway
against the OpenAI spec will find these surprising, and might otherwise "fix"
them. Each is a decision, not an oversight.

## A request without `model` is served using a default

The OpenAI schema marks `model` required. This gateway fills in
`config.DefaultModelID` (currently `GLM-5.3`) and serves the request.

Rationale: this is a convenience proxy, not the OpenAI API. Requiring `model` on
every request only makes clients harder to write. Consequence: a malformed client
never learns its request is malformed — it receives a perfectly good completion
attributed to a model it never asked for.

Reversing this is not a one-line change. Clients grow to rely on the default, so
tightening it later breaks them. The async path (`/async/v1/messages`) bypasses
the default altogether, so any change here has to align both paths in the same
commit or they will disagree about what a model-less request means.

## `response_format` is accepted and dropped — unless it is strict

Requests carrying `json_object`, or `json_schema` without
`response_format.json_schema.strict` set to `true`, succeed with the field
discarded and a warning logged. The model may still return JSON if its prompt
suggests it, but no schema constrains it. Anything we cannot parse is treated
the same way: warn and serve, since rejecting over a field we were going to
discard anyway would break clients for no benefit.

`response_format.json_schema.strict: true` is different: the client is
explicitly contracting for schema conformance we cannot provide, so the request
is refused with `400` and an OpenAI-shaped error rather than answered with prose
the client will fail to parse.

Related: ADR-0001 records why the gateway does not emulate structured output.

## `completion_tokens_details` is never emitted

There is no trustworthy source for `reasoning_tokens` in what the Origin
reports. A zero would read as "measured, none used" rather than "not measured",
so the object is omitted.

## `usage` is omitted rather than zeroed

The same rule, applied to the whole object: when nothing was measured, the
`usage` key is absent rather than a set of zeros.
