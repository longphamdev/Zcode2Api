# No structured-output emulation at the gateway

Status: accepted

A client can ask for a JSON Schema through OpenAI's `response_format`, but the
Origin does not enforce schemas, so the gateway does not pretend otherwise.

The field has no equivalent in the Anthropic wire format. The nearest one,
`output_config.format`, is forwarded untouched and then ignored by the Origin:
repeated runs with a demanding schema (`enum`, `required`,
`additionalProperties: false`) came back as prose, while a control prompt with
no schema returned clean JSON on demand. The gap is in the Origin, not in this
gateway.

We rejected prompt-level coercion with fence stripping: it rewrites the client's
own prompt, so it can disturb instructions unrelated to the schema; it cannot
prove conformance, since a fence-stripped reply may still violate the schema;
and it only works when the model cooperates, which is not a guarantee. We also
rejected silently forwarding the field, which changes nothing about the response
while leaving the client believing it was honoured.

A client asking for structured output therefore gets no guarantee. A request
that sets `response_format.json_schema.strict` to `true` is refused outright
rather than answered with prose — the client was contracting for conformance, so
it gets an error instead of a silent substitution. Everything less explicit is
served with the field dropped and a warning logged. See `docs/deviations.md`.

Revisit when the Origin starts enforcing the schema — checkable with the same
control test — or when the gateway gains a trustworthy enforcement mechanism:
validating the reply against the schema and returning an error rather than
passing bad content through.
