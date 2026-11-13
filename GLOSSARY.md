# Zcode2Api

An OpenAI- and Anthropic-compatible gateway in front of a Z.AI account pool. Clients speak either wire format; the gateway forwards to Z.AI and translates where the formats differ.

## Language

**Origin**:
The Z.AI service this gateway forwards requests to, which answers in Anthropic Messages format. Runs at a fixed base URL.
_Avoid_: Upstream, provider, backend, server

**Upstream repository**:
The GitHub project this repository was forked from (`D3-vin/Zcode2Api`). A source of code, not a service.
_Avoid_: Origin, upstream

**`internal/upstream`**:
A package in this codebase. It talks to the Origin. The name is historical and refers to nothing else.
_Avoid_: Upstream

**Account**:
One credential in the pool — a JWT plus its device identifier — against which the Origin meters quota. Several accounts share the pool and are failed over between.
_Avoid_: Key, token, user, seat

**Pool**:
The set of enabled Accounts a request may be routed to. Distinct from any single Account.
_Avoid_: Farm, list, queue

**Warm**:
The admission sequence a new Account goes through — activation events, plan preview, claim — before it serves client traffic.
_Avoid_: Init, setup, bootstrap

**Claim**:
Asking the Origin to grant a plan an Account is offered. Success is per-plan and can be refused by eligibility, quota or captcha.
_Avoid_: Redeem, activate, subscribe

**Egress**:
The network line a request leaves through. Accounts can be bound to one; requests rotate egress on a load verdict.
_Avoid_: Proxy, route, exit

**Chat dump**:
A diagnostic capture of both the client's request body and the payload sent to the Origin, written to a file when enabled.
_Avoid_: Log, trace, capture
