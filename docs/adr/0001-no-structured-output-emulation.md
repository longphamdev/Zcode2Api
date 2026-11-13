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

A client asking for structured output therefore gets no guarantee. See
`docs/deviations.md` for what happens to `response_format` on the wire today, and
issue #13 for the strict-mode case.

Revisit when the Origin starts enforcing the schema — checkable with the same
control test — or when the gateway gains a trustworthy enforcement mechanism:
validating the reply against the schema and returning an error rather than
passing bad content through.
