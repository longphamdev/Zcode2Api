# Zcode2Api

An OpenAI- and Anthropic-compatible gateway in front of a Z.AI account pool. Clients speak either wire format; the gateway forwards to Z.AI and translates where the formats differ.

## Language

> **A naming trap, read this first.** Three different things are called "origin"
> or "upstream" in this project, and they point in opposite directions:
>
> | Word | Means | Where it appears |
> | --- | --- | --- |
> | **Origin** (capitalised) | the Z.AI service — the thing we call out to | this glossary, code comments, ADRs |
> | **origin** (git remote) | *our* fork, `longphamdev/Zcode2Api` — where we push | `git remote -v` |
> | **upstream** (git remote) | the project we forked from, `D3-vin/Zcode2Api` | `git remote -v` |
> | **`internal/upstream`** | a package name; it talks to the Origin | the source tree |
>
> From the gateway's point of view Z.AI is upstream (it is the service we depend
> on). From git's point of view our fork is `origin` and Z.AI's home repo is
> `upstream`. Both are correct in their own frame. Prefer **Origin** when
> speaking about the API, and always say *git remote* when speaking about
> `origin`/`upstream` in the git sense.

**Origin**:
The Z.AI service the gateway calls out to; it answers in Anthropic Messages format and meters quota.
_Avoid_: Upstream, provider, backend, server

**Upstream repository**:
The project this one was forked from — a code lineage, never a running service.
_Avoid_: Origin, the origin

**`internal/upstream`**:
The package that talks to the Origin. The name is a historical artefact and does not refer to the Upstream repository.
_Avoid_: Upstream (as a shorthand for the Origin)

**Account**:
One credential in the Pool — a JWT and its device identifier — that the Origin meters quota against.
_Avoid_: Key, token, user, seat

**Pool**:
The set of Accounts a request may be routed to. Note the word is overloaded in code: an *egress pool* and the *captcha pool* are separate collections of proxies and tokens.
_Avoid_: Farm, list, queue

**Warm**:
The admission sequence an Account completes before it serves client traffic.
_Avoid_: Init, setup, bootstrap

**Claim**:
A request for the Origin to grant a plan an Account has been offered.
_Avoid_: Redeem, activate, subscribe

**Egress**:
The network route a request leaves by, which can be bound per Account.
_Avoid_: Proxy, route, exit

**Chat dump**:
A diagnostic capture of a client's request alongside the payload sent to the Origin.
_Avoid_: Log, trace, capture
