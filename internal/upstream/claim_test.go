package upstream

// Characterization tests for the claim path (#23).
//
// These pin what ClaimOfferedPlans and claimPlan do TODAY, at the wire level,
// so that #24 (diagnostics), #25 (marketing banner) and #26 (mint resilience)
// can prove they did not regress it. internal/upstream had no tests before
// this file.
//
// Each test names itself either "existing behaviour" — recording what the code
// does, including what it does wrong — or "expected contract" — an invariant
// that must survive the coming refactor. Nothing here is allowed to launder a
// bug into a requirement: where the current behaviour is wrong, the test says
// so in its comment and asserts the wrong thing explicitly, so a later fix
// makes it fail loudly instead of silently passing.
//
// OriginBaseURL is a package-level var (config/config.go:14-16 says it is a var
// "so tests can point them at a mock upstream"), so every test restores it and
// none of them run in parallel.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"zcode2api/internal/config"
)

// originStub is a fake Origin that routes by path and records what it saw.
type originStub struct {
	t *testing.T

	mu       sync.Mutex
	requests []recordedRequest
	// route returns (status, body) for a path. A nil route answers 404.
	route func(path string, body map[string]any) (int, string)
}

type recordedRequest struct {
	Method string
	Path   string
	Query  map[string]string
	Header http.Header
	Body   map[string]any
	Raw    string
}

func (o *originStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	q := map[string]string{}
	for k, v := range r.URL.Query() {
		q[k] = v[0]
	}
	o.mu.Lock()
	o.requests = append(o.requests, recordedRequest{
		Method: r.Method, Path: r.URL.Path, Query: q,
		Header: r.Header.Clone(), Body: body, Raw: string(raw),
	})
	o.mu.Unlock()

	status, out := http.StatusNotFound, `{"code":404,"msg":"no route"}`
	if o.route != nil {
		if s, b := o.route(r.URL.Path, body); s != 0 {
			status, out = s, b
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, out)
}

func (o *originStub) calls() []recordedRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]recordedRequest, len(o.requests))
	copy(out, o.requests)
	return out
}

// callsTo returns every recorded request whose path contains want.
func (o *originStub) callsTo(want string) []recordedRequest {
	var out []recordedRequest
	for _, r := range o.calls() {
		if strings.Contains(r.Path, want) {
			out = append(out, r)
		}
	}
	return out
}

// newStub points config.OriginBaseURL at a fake Origin for the duration of the
// test. Not parallel-safe by design: it mutates a package-level global.
func newStub(t *testing.T, route func(path string, body map[string]any) (int, string)) (*Client, *originStub) {
	t.Helper()
	stub := &originStub{t: t, route: route}
	srv := httptest.NewServer(stub)
	prev := config.OriginBaseURL
	config.OriginBaseURL = srv.URL
	t.Cleanup(func() {
		config.OriginBaseURL = prev
		srv.Close()
	})
	return &Client{}, stub
}

// previewBody builds a preview answer in the shape the Origin actually sends.
func previewBody(plans ...string) string {
	type plan struct {
		PlanID   string `json:"plan_id"`
		Name     string `json:"name"`
		Priority int    `json:"priority"`
	}
	var ps []plan
	for i, p := range plans {
		ps = append(ps, plan{PlanID: p, Name: p, Priority: len(plans) - i})
	}
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"plans": ps}})
	return string(raw)
}

// TestClaimRequestsPreviewWithBillingIdentity — existing behaviour.
//
// The preview call carries the billing identity: app_version and platform in
// the query, and the billing header set on the request. #25 will add a second
// face; it must not change what the first one asks for.
func TestClaimRequestsPreviewWithBillingIdentity(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody()
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		}
		return 0, ""
	})

	_, _ = c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	previews := stub.callsTo("/billing/preview")
	if len(previews) != 1 {
		t.Fatalf("preview called %d times, want 1", len(previews))
	}
	p := previews[0]
	if p.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", p.Method)
	}
	if got := p.Query["app_version"]; got != config.BillingClientVersion {
		t.Errorf("app_version = %q, want %q", got, config.BillingClientVersion)
	}
	if got := p.Query["platform"]; got != config.ClientPlatform {
		t.Errorf("platform = %q, want %q", got, config.ClientPlatform)
	}
	if got := p.Header.Get("Authorization"); got != "Bearer jwt.test" {
		t.Errorf("Authorization = %q, want the account's JWT", got)
	}
	if got := p.Header.Get("X-Device-Mid"); got != "mid-test" {
		t.Errorf("X-Device-Mid = %q, want the account's device id", got)
	}
}

// TestClaimEmptyPreviewYieldsNothingOffered — existing behaviour, and the bug.
//
// The measured shape: code 0 with NO data field. The parser reads
// data.plans only, so this is indistinguishable from "the Origin offers no
// campaign" — which is exactly what it reports. It is also what a
// version-floor rejection and a banner-only campaign both look like.
//
// #24 must make these three distinguishable. When it does, this test should
// FAIL — that is the point: the assertion records today's silence so the fix
// cannot land unnoticed.
func TestClaimEmptyPreviewYieldsNothingOffered(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, `{"code":0,"msg":"","logid":"20261113164707d5ebdda3ed8a7e466079"}`
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	if claimed != "" || problems != "" {
		t.Fatalf("claimed=%q problems=%q; today a preview with no data reports nothing at all", claimed, problems)
	}
	if claimed == "" && problems == "" {
		t.Log("EXISTING BEHAVIOUR, KNOWN BUG: a banner-only campaign and a version-floor rejection " +
			"both report as complete silence. #24 removes this silence.")
	}
}

// TestClaimRejectedPreviewReportsStatusAndSnip — existing behaviour.
//
// A non-200 preview is a problem string carrying the status and a body snippet.
// #24 keeps this but must stop it being conflated with an empty campaign.
func TestClaimRejectedPreviewReportsStatusAndSnip(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		if strings.HasSuffix(path, "/billing/preview") {
			return 403, `{"code":3012,"msg":"blocked"}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	if claimed != "" {
		t.Errorf("claimed = %q, want empty when the preview was refused", claimed)
	}
	if !strings.Contains(problems, "403") {
		t.Errorf("problems = %q, want the HTTP status in it", problems)
	}
	if !strings.Contains(problems, "3012") {
		t.Errorf("problems = %q, want the business code visible too", problems)
	}
}

// TestClaimPreviewNonZeroCodeIsReported — existing behaviour.
func TestClaimPreviewNonZeroCodeIsReported(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		if strings.HasSuffix(path, "/billing/preview") {
			return 200, `{"code":1004,"msg":"ineligible"}`
		}
		return 0, ""
	})

	_, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if !strings.Contains(problems, "1004") {
		t.Errorf("problems = %q, want the business code 1004 in it", problems)
	}
}

// TestClaimPostsThePlanIDAndCaptchaHeaders — existing behaviour.
//
// The claim POST carries {"plan_id": ...} and, when a solver is present, the
// two Aliyun headers. #26 changes *when* those headers are sent, never what
// they are called.
func TestClaimPostsThePlanIDAndCaptchaHeaders(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-alpha")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"plan-alpha"}}}`
		}
		return 0, ""
	})
	c.Captcha = fakeSolver{param: "captcha-param-xyz", region: "cn"}

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Fatalf("problems = %q, want none", problems)
	}
	if !strings.Contains(claimed, "plan-alpha") {
		t.Errorf("claimed = %q, want the plan reported as claimed", claimed)
	}

	claims := stub.callsTo("/billing/claim")
	if len(claims) != 1 {
		t.Fatalf("claim POSTed %d times, want 1", len(claims))
	}
	cl := claims[0]
	if cl.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", cl.Method)
	}
	if got, _ := cl.Body["plan_id"].(string); got != "plan-alpha" {
		t.Errorf("plan_id = %q, want plan-alpha", got)
	}
	if got := cl.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "captcha-param-xyz" {
		t.Errorf("captcha param header = %q, want the minted param", got)
	}
	if got := cl.Header.Get("X-Aliyun-Captcha-Verify-Region"); got != "cn" {
		t.Errorf("captcha region header = %q, want the minted region", got)
	}
}

// TestClaimAlreadyOwnedPlanIsSkipped — existing behaviour, and a design defect
// handed to #24.
//
// A plan id the billing snapshot already reports as owned is not re-claimed —
// that idempotence is what the daily warm loop relies on.
//
// But the "nothing to do" outcome is transported through the *problems* string
// ("already have: ..."), so every caller has to recognise it by matching a
// literal prefix (`internal/gateway/dashboard.go:326` and `:381`) to avoid
// showing a healthy Account as broken. #24 exists to give these outcomes
// distinct names; when it does, this assertion should change.
func TestClaimAlreadyOwnedPlanIsSkipped(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-owned")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"plan-owned"}]}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	if n := len(stub.callsTo("/billing/claim")); n != 0 {
		t.Fatalf("claim POSTed %d times for an owned plan, want 0", n)
	}
	if claimed != "" {
		t.Errorf("claimed = %q, want empty — nothing was claimed, the plan was already owned", claimed)
	}
	if !strings.HasPrefix(problems, "already have:") {
		t.Errorf("problems = %q; today an owned-everything Account reports through the problems "+
			"string with an \"already have:\" prefix, which callers match literally", problems)
	}
	t.Log("EXISTING BEHAVIOUR, DESIGN DEFECT FOR #24: \"already owned\" is a non-problem " +
		"transported in the problems string, recognised by callers via a string prefix.")
}

// TestClaimPlanLessCodeZeroIsNotASuccess — the contract, flipped.
//
// This test previously pinned code 0 with no plan document as a successful
// claim, and logged it as a correctness question. The reference client requires
// a plan document before it believes a claim landed, and it is right: a bare
// code 0 is indistinguishable from a no-op, so reporting it as claimed tells
// the operator a plan was granted when nothing proves one was.
//
// Deliberately NOT merged with the 1003 test below: alreadyClaimed is a genuine
// success that carries no plan document, so the two codes must stay separable.
func TestClaimPlanLessCodeZeroIsNotASuccess(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-beta")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			// code 0, but NO data.plan — nothing was granted.
			return 200, `{"code":0,"msg":""}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	if claimed != "" {
		t.Errorf("claimed = %q; a plan-less code 0 must not be reported as a claimed plan", claimed)
	}
	if !strings.Contains(problems, "code 0") {
		t.Errorf("problems = %q, want the plan-less code 0 reported as a failure", problems)
	}
}

// TestClaimCodeZeroWithPlanDocumentIsASuccess — the other half of the same
// contract: code 0 IS a claim when the Origin names the granted plan.
func TestClaimCodeZeroWithPlanDocumentIsASuccess(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-withdoc")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"plan-withdoc"}}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Errorf("problems = %q, want none when the plan document is present", problems)
	}
	if !strings.Contains(claimed, "plan-withdoc") {
		t.Errorf("claimed = %q, want the granted plan", claimed)
	}
}

// TestClaimAlreadyClaimedCodeIsAccepted — existing behaviour, PINNED SEPARATELY.
//
// 1003 is alreadyClaimed on the wire (the reference client classifies it as
// "already_claimed"). Treating it as success may well be correct: the account
// has the plan, which is the outcome the caller wants. Kept apart from the
// code-0 test for the reason given there.
func TestClaimAlreadyClaimedCodeIsAccepted(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-gamma")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":1003,"msg":"already claimed"}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Errorf("problems = %q; 1003 alreadyClaimed is accepted today", problems)
	}
	if !strings.Contains(claimed, "plan-gamma") {
		t.Errorf("claimed = %q; 1003 alreadyClaimed is reported as claimed today", claimed)
	}
}

// TestClaimCaptchaRejectedRetriesExactlyOnce — existing behaviour.
//
// A 3007 on the claim path is retried once with a freshly minted param, then
// given up on. #26 must not turn this into a loop, and must not re-implement
// it as new work.
func TestClaimCaptchaRejectedRetriesExactlyOnce(t *testing.T) {
	solver := &countingSolver{param: "p1", region: "cn"}
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-delta")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":3007,"msg":"captcha verify failed"}`
		}
		return 0, ""
	})
	c.Captcha = solver

	_, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	if n := len(stub.callsTo("/billing/claim")); n != 2 {
		t.Fatalf("claim POSTed %d times on a persistent 3007, want exactly 2 (one retry)", n)
	}
	if solver.calls != 2 {
		t.Errorf("solver minted %d times, want 2 — one per attempt", solver.calls)
	}
	if !strings.Contains(problems, "3007") {
		t.Errorf("problems = %q, want the 3007 reported after the retry failed", problems)
	}
}

// TestClaimCaptchaRejectedThenAccepted — existing behaviour.
//
// The retry is the useful one: a fresh param on the second attempt can succeed.
func TestClaimCaptchaRejectedThenAccepted(t *testing.T) {
	var attempt int
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-epsilon")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			attempt++
			if attempt == 1 {
				return 200, `{"code":3007,"msg":"captcha verify failed"}`
			}
			return 200, `{"code":0,"data":{"plan":{"plan_id":"plan-epsilon"}}}`
		}
		return 0, ""
	})
	c.Captcha = fakeSolver{param: "p", region: "cn"}

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Errorf("problems = %q, want none after the retry succeeded", problems)
	}
	if !strings.Contains(claimed, "plan-epsilon") {
		t.Errorf("claimed = %q, want the plan claimed on the retry", claimed)
	}
	if n := len(stub.callsTo("/billing/claim")); n != 2 {
		t.Errorf("claim POSTed %d times, want 2", n)
	}
}

// TestClaimSendsHighestPriorityFirst — existing behaviour.
//
// The offer list is sorted by priority descending before claiming, so the
// most valuable plan is attempted first. #25's merge must preserve the sort
// across the two faces.
func TestClaimSendsHighestPriorityFirst(t *testing.T) {
	c, stub := newStub(t, func(path string, body map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, `{"code":0,"data":{"plans":[
				{"plan_id":"low","name":"low","priority":1},
				{"plan_id":"high","name":"high","priority":99}
			]}}`
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"` + body["plan_id"].(string) + `"}}}`
		}
		return 0, ""
	})

	_, _ = c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	claims := stub.callsTo("/billing/claim")
	if len(claims) != 2 {
		t.Fatalf("claimed %d plans, want 2", len(claims))
	}
	if got, _ := claims[0].Body["plan_id"].(string); got != "high" {
		t.Errorf("first claim = %q, want high (priority 99) first", got)
	}
}

// TestClaimSolverAbsentStillPosts — existing behaviour.
//
// With no solver configured the claim is still attempted, without captcha
// headers. This is the wire shape the captcha gate is allowed to decide on
// (#26); today the decision is simply "no solver, no header".
func TestClaimSolverAbsentStillPosts(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-zeta")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"plan-zeta"}}}`
		}
		return 0, ""
	})
	c.Captcha = nil

	_, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Fatalf("problems = %q, want none with no solver", problems)
	}
	claims := stub.callsTo("/billing/claim")
	if len(claims) != 1 {
		t.Fatalf("claim POSTed %d times, want 1", len(claims))
	}
	if got := claims[0].Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "" {
		t.Errorf("captcha param = %q, want none when no solver is configured", got)
	}
}

// TestClaimMintFailureAbortsWithoutPosting — existing behaviour, and the wall #26 removes.
//
// A solver that cannot mint returns before the claim is POSTed. #26 must change
// this: a gate-off campaign should still get a claim, sent without headers.
func TestClaimMintFailureAbortsWithoutPosting(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, previewBody("plan-eta")
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		}
		return 0, ""
	})
	c.Captcha = failingSolver{}

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	if n := len(stub.callsTo("/billing/claim")); n != 0 {
		t.Errorf("claim POSTed %d times despite a mint failure, want 0", n)
	}
	if claimed != "" {
		t.Errorf("claimed = %q, want empty", claimed)
	}
	if !strings.Contains(problems, "mint") {
		t.Errorf("problems = %q, want the mint failure reported", problems)
	}
	t.Log("EXISTING BEHAVIOUR, THE WALL #26 REMOVES: a mint failure aborts the claim entirely.")
}

// --- fakes -----------------------------------------------------------------

type fakeSolver struct{ param, region string }

func (f fakeSolver) Solve(context.Context) (string, string, error) { return f.param, f.region, nil }
func (f fakeSolver) Close()                                        {}

type countingSolver struct {
	param, region string
	calls         int
}

func (c *countingSolver) Solve(context.Context) (string, string, error) {
	c.calls++
	return c.param, c.region, nil
}

func (c *countingSolver) Close() {}

type failingSolver struct{}

func (failingSolver) Solve(context.Context) (string, string, error) {
	return "", "", io.ErrUnexpectedEOF
}

func (failingSolver) Close() {}

// --- the marketing banner face ---------------------------------------------

// bannerBody builds a marketing/touch answer in the shape the Origin sends:
// the plan id lives on the button action, the full plan object on the
// background. Taken from the payload measured 2026-11-13.
func bannerBody(planID, name string, priority int) string {
	return `{"code":0,"data":{"deliveries":[{"campaign_id":"2101141259813175296",
		"banner":{"buttons":[
			{"text":{"format":"","content":""},"action":{"type":"close","args":null}},
			{"text":{"format":"","content":""},"action":{"type":"claim_zcode_plan","args":{"plan_id":"` + planID + `"}}}
		],
		"background":{"type":"bundle","args":{"zcode_plan":{
			"plan_id":"` + planID + `","name":"` + name + `","priority":` + itoa(priority) + `,
			"starts_at":1794567600,"ends_at":1794790800}}}}}]}}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestBannerPlanIsOfferedWhenPreviewIsEmpty — the actual bug.
//
// Measured production shape: preview answers {"code":0} with no data, and the
// claimable plan exists only on the marketing banner. Before this, that
// combination reported "nothing offered" and the plan was never claimed.
func TestBannerPlanIsOfferedWhenPreviewIsEmpty(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, `{"code":0,"msg":"","logid":"20261113164707d5ebdda3ed8a7e466079"}`
		case strings.HasSuffix(path, "/marketing/touch"):
			return 200, bannerBody("zcode-v3-start-plan-1113-wk", "ZCODE WEEKEND BUILD", 101)
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"zcode-v3-start-plan-1113-wk"}}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	if problems != "" {
		t.Fatalf("problems = %q, want none", problems)
	}
	if !strings.Contains(claimed, "ZCODE WEEKEND BUILD") {
		t.Fatalf("claimed = %q; the banner-only plan was not offered", claimed)
	}
	claims := stub.callsTo("/billing/claim")
	if len(claims) != 1 {
		t.Fatalf("claim POSTed %d times, want 1", len(claims))
	}
	if got, _ := claims[0].Body["plan_id"].(string); got != "zcode-v3-start-plan-1113-wk" {
		t.Errorf("plan_id = %q, want the banner's plan", got)
	}
}

// TestBannerRequestCarriesItsHeaders — the marketing face needs its own set:
// a device id (UUID-parsed by the Origin), the locale, the app version, and the
// JWT when there is one.
func TestBannerRequestCarriesItsHeaders(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, `{"code":0}`
		case strings.HasSuffix(path, "/marketing/touch"):
			return 200, bannerBody("plan-x", "Plan X", 5)
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"plan-x"}}}`
		}
		return 0, ""
	})

	_, _ = c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")

	calls := stub.callsTo("/marketing/touch")
	if len(calls) != 1 {
		t.Fatalf("marketing/touch called %d times, want 1", len(calls))
	}
	m := calls[0]
	if got := m.Header.Get("X-Device-Mid"); got != "mid-test" {
		t.Errorf("X-Device-Mid = %q, want the account's device id", got)
	}
	if got := m.Header.Get("X-Client-Language"); got == "" {
		t.Error("X-Client-Language is empty; the marketing face wants a locale")
	}
	if got := m.Header.Get("X-ZCode-App-Version"); got != config.BillingClientVersion {
		t.Errorf("X-ZCode-App-Version = %q, want %q", got, config.BillingClientVersion)
	}
	if got := m.Header.Get("Authorization"); got != "Bearer jwt.test" {
		t.Errorf("Authorization = %q, want the account's JWT", got)
	}
	if got := m.Query["seq"]; got == "" {
		t.Error("seq is missing; the banner endpoint takes a sequence number")
	}
}

// TestPreviewWinsOnASharedPlanID — the merge rule.
//
// Both faces offering the same plan id must not claim it twice, and the
// preview's priority must survive: it is the face that owns the authoritative
// window and entitlements.
func TestPreviewWinsOnASharedPlanID(t *testing.T) {
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			// Preview offers "shared" at priority 50.
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"shared","name":"Shared","priority":50}]}}`
		case strings.HasSuffix(path, "/marketing/touch"):
			// Banner offers the same id at priority 999 — must not win.
			return 200, bannerBody("shared", "Shared banner", 999)
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"shared"}}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Fatalf("problems = %q", problems)
	}
	if claims := stub.callsTo("/billing/claim"); len(claims) != 1 {
		t.Errorf("claim POSTed %d times for one shared plan id, want exactly 1", len(claims))
	}
	// The preview's name wins, proving the preview record was kept.
	if !strings.Contains(claimed, "Shared") || strings.Contains(claimed, "Shared banner") {
		t.Errorf("claimed = %q; the preview's record should win on a shared plan id", claimed)
	}
}

// TestBannerFailureFallsBackToPreview — a marketing outage must never turn a
// working preview into "no plans offered". Trading one silent failure for
// another is the specific mistake this guards.
func TestBannerFailureFallsBackToPreview(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"from-preview","name":"From Preview","priority":10}]}}`
		case strings.HasSuffix(path, "/marketing/touch"):
			return 500, `{"code":500,"msg":"boom"}`
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"from-preview"}}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Errorf("problems = %q; a banner failure must not surface as a problem", problems)
	}
	if !strings.Contains(claimed, "From Preview") {
		t.Errorf("claimed = %q; the preview plan must still be claimed when the banner fails", claimed)
	}
}

// TestBannerWithoutPlanObjectStillOffers — the button id is authoritative.
// The measured payload puts priority on the plan object, not the button, so a
// button-only delivery cannot be ranked — but it must still be offered.
func TestBannerWithoutPlanObjectStillOffers(t *testing.T) {
	body := `{"code":0,"data":{"deliveries":[{"banner":{"buttons":[
		{"action":{"type":"claim_zcode_plan","args":{"plan_id":"bare-id"}}}]}}]}}`
	c, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 200, `{"code":0}`
		case strings.HasSuffix(path, "/marketing/touch"):
			return 200, body
		case strings.HasSuffix(path, balancePath):
			return 200, `{"code":0,"data":{"plans":[]}}`
		case strings.HasSuffix(path, "/billing/claim"):
			return 200, `{"code":0,"data":{"plan":{"plan_id":"bare-id"}}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt.test", "mid-test")
	if problems != "" {
		t.Fatalf("problems = %q", problems)
	}
	if !strings.Contains(claimed, "bare-id") {
		t.Errorf("claimed = %q; a button id with no plan object must still be offered", claimed)
	}
	if claims := stub.callsTo("/billing/claim"); len(claims) != 1 {
		t.Errorf("claim POSTed %d times, want 1", len(claims))
	}
}

// TestPreviewFailureStillReadsTheBanner — the same class of bug as the banner
// gap itself, one layer up.
//
// The banner is most likely to be the only face carrying a campaign exactly
// when the preview is dark: pre-launch (404), or a parameter error (3001). If a
// failed preview ended the walk, those campaigns would stay invisible — which is
// what this did before, even after the banner face was added.
func TestPreviewFailureStillReadsTheBanner(t *testing.T) {
	for _, c := range []struct {
		name         string
		previewCode  int
		previewBody  string
		wantReadable bool
	}{
		{"preview 404", 404, `{"code":404,"msg":"not deployed"}`, true},
		{"preview 3001 parameter error", 200, `{"code":3001,"msg":"parameter error"}`, true},
		{"preview 500", 500, `{"code":500}`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Assert the banner is actually consulted, which is the behaviour
			// that matters: the offer then flows through normal claiming.
			srv, stub := newStub(t, func(path string, _ map[string]any) (int, string) {
				switch {
				case strings.HasSuffix(path, "/billing/preview"):
					return c.previewCode, c.previewBody
				case strings.HasSuffix(path, "/marketing/touch"):
					return 200, bannerBody("banner-plan", "Banner Plan", 100)
				case strings.HasSuffix(path, balancePath):
					return 200, `{"code":0,"data":{"plans":[]}}`
				case strings.HasSuffix(path, "/billing/claim"):
					return 200, `{"code":0,"data":{"plan":{"plan_id":"banner-plan"}}}`
				}
				return 0, ""
			})

			claimed, problems := srv.ClaimOfferedPlans(context.Background(), "jwt", "mid")
			if n := len(stub.callsTo("/marketing/touch")); n != 1 {
				t.Fatalf("marketing/touch called %d times, want 1 — a failed preview must not "+
					"end the walk before the banner is read", n)
			}
			if problems != "" {
				t.Errorf("problems = %q, want none: the banner offered a claimable plan", problems)
			}
			if !strings.Contains(claimed, "Banner Plan") {
				t.Errorf("claimed = %q, want the banner plan", claimed)
			}
		})
	}
}

// TestBothFacesEmptyReportsThePreviewProblem — when neither face offers
// anything AND the preview failed, the preview's reason must surface. Reporting
// a clean "nothing offered" there would hide an Origin problem.
func TestBothFacesEmptyReportsThePreviewProblem(t *testing.T) {
	c, _ := newStub(t, func(path string, _ map[string]any) (int, string) {
		switch {
		case strings.HasSuffix(path, "/billing/preview"):
			return 403, `{"code":3012,"msg":"blocked"}`
		case strings.HasSuffix(path, "/marketing/touch"):
			return 200, `{"code":0,"data":{"deliveries":[]}}`
		}
		return 0, ""
	})

	claimed, problems := c.ClaimOfferedPlans(context.Background(), "jwt", "mid")
	if claimed != "" {
		t.Errorf("claimed = %q, want empty", claimed)
	}
	if !strings.Contains(problems, "403") {
		t.Errorf("problems = %q, want the preview's own failure surfaced", problems)
	}
}
