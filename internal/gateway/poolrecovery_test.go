package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

func TestRecoveryAtReadyWhenAccountServable(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{ID: 1}}

	if until, _, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Errorf("reported a wait of %s, but an account is servable now", until)
	}
}

func TestRecoveryAtReportsEarliestCooldown(t *testing.T) {
	now := time.Now()
	accs := []store.Account{
		{ID: 1, CoolingUntil: now.Add(16 * time.Minute).Unix()},
		{ID: 2, CoolingUntil: now.Add(13 * time.Minute).Unix()},
	}

	until, readyNow, waiting := recoveryAt(accs, nil, "GLM-5.3", now)
	_ = readyNow
	if !waiting {
		t.Fatal("reported no wait, but every account is cooling")
	}
	if d := until.Sub(now); d < 12*time.Minute || d > 14*time.Minute {
		t.Errorf("recovery in %s, want about 13m (the earlier of the two)", d)
	}
}

func TestRecoveryAtEmptyPoolIsNotAWait(t *testing.T) {
	if _, _, waiting := recoveryAt(nil, nil, "GLM-5.3", time.Now()); waiting {
		t.Error("an empty pool is nothing to wait for")
	}
}

func TestRecoveryAtAllExpiredIsNotAWait(t *testing.T) {
	now := time.Now()
	// Each carries a future CoolingUntil as well, so the test fails if Status
	// stops being consulted — a bare expired row would pass either way because
	// its zero CoolingUntil already yields no wait.
	accs := []store.Account{
		{ID: 1, Status: "expired", CoolingUntil: now.Add(10 * time.Minute).Unix()},
		{ID: 2, Status: "revoked", CoolingUntil: now.Add(20 * time.Minute).Unix()},
	}

	if until, _, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Errorf("status marks an account as never recovering; got a wait until %s", until)
	}
}

func TestRecoveryAtRespectsDeadMarker(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{ID: 1}}
	dead := map[string]int64{"1:glm53": now.Add(20 * time.Minute).Unix()}

	until, _, waiting := recoveryAt(accs, dead, "GLM-5.3", now)
	if !waiting {
		t.Fatal("a model-dead account should report a wait")
	}
	if d := until.Sub(now); d < 19*time.Minute {
		t.Errorf("recovery in %s, want ~20m from the dead marker", d)
	}
}

// A servable account beats any wait: the pool is usable now even though another
// account is benched.
func TestRecoveryAtServableBeatsCooldown(t *testing.T) {
	now := time.Now()
	accs := []store.Account{
		{ID: 1, CoolingUntil: now.Add(30 * time.Minute).Unix()},
		{ID: 2},
	}

	if _, _, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Error("a servable account means no wait is needed")
	}
}

// An account excluded for this model does not contribute a recovery time for it.
func TestRecoveryAtIgnoresModelExcluded(t *testing.T) {
	now := time.Now()
	accs := []store.Account{
		{ID: 1, CoolingUntil: now.Add(10 * time.Minute).Unix(), DisabledModels: "glm53"},
	}

	if _, _, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Error("an account excluded for this model never recovers for it")
	}
}

// Three outcomes, not two. "Servable right now" must NOT short-circuit the
// hold — a walk that just failed transiently leaves every Account servable, and
// the next pass may succeed. Only "nothing will ever recover" fails fast.
func TestPoolOutcomeDistinguishesServableFromHopeless(t *testing.T) {
	now := time.Now()
	holdUntil := now.Add(10 * time.Minute)

	cases := []struct {
		name string
		accs []store.Account
		dead map[string]int64
		want poolOutcome
	}{
		{
			name: "servable account after a transient failure",
			accs: []store.Account{{ID: 1}},
			want: poolReadyNow,
		},
		{
			name: "cooling inside the window",
			accs: []store.Account{{ID: 1, CoolingUntil: now.Add(2 * time.Minute).Unix()}},
			want: poolRecoveringSoon,
		},
		{
			name: "cooling beyond the window",
			accs: []store.Account{{ID: 1, CoolingUntil: now.Add(16 * time.Minute).Unix()}},
			want: poolHopeless,
		},
		{
			name: "all expired",
			accs: []store.Account{{ID: 1, Status: "expired"}},
			want: poolHopeless,
		},
		{
			name: "empty",
			accs: nil,
			want: poolHopeless,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := poolAvailability(c.accs, c.dead, "GLM-5.3", now, holdUntil)
			if got != c.want {
				t.Errorf("outcome = %v, want %v", got, c.want)
			}
		})
	}
}

// A model-dead marker is scoped to one model. An Account dead for 20 minutes on
// a long-scope model but 2 minutes on another must not report the 2-minute
// recovery to a request for the long-scope model — that would hold a window it
// cannot recover in, and advertise a false Retry-After.
func TestRecoveryAtIgnoresOtherModelsDeadMarkers(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{ID: 1}}
	dead := map[string]int64{
		"1:glm53":      now.Add(20 * time.Minute).Unix(),
		"1:glm53flash": now.Add(2 * time.Minute).Unix(),
	}

	until, _, waiting := recoveryAt(accs, dead, "GLM-5.3", now)
	if !waiting {
		t.Fatal("a model-dead account should report a wait for that model")
	}
	if d := until.Sub(now); d < 19*time.Minute {
		t.Errorf("recovery in %s for glm53, want ~20m — another model's marker leaked in", d)
	}
}

// An Account whose plan does not include this model can never serve it, so it
// must not contribute a recovery time — pickExcept already refuses it.
func TestRecoveryAtIgnoresPlanWithoutModel(t *testing.T) {
	now := time.Now()
	// The plan was scanned and does not carry this model at all.
	// Cooling as well, so the exclusion is the only thing that can explain a
	// correct answer — otherwise the account would simply read as servable.
	accs := []store.Account{{
		ID:           1,
		CoolingUntil: now.Add(5 * time.Minute).Unix(),
		QuotaJSON:    `[{"name":"other-model","limit":100,"remaining":100}]`,
	}}

	if until, _, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Errorf("an account whose plan lacks this model never recovers for it; got a wait until %s", until)
	}
}

// Retry-After must not round down to zero for a recovery that is still in the
// future: a client told "0" retries immediately, into the same closed pool.
func TestRetryAfterRoundsUp(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		recover time.Time
		want    string
	}{
		{"sub-second remainder", now.Add(400 * time.Millisecond), "1"},
		{"just over a second", now.Add(1100 * time.Millisecond), "2"},
		{"whole seconds", now.Add(30 * time.Second), "30"},
		{"already past", now.Add(-5 * time.Second), "0"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryAfterFrom(c.recover, now); got != c.want {
				t.Errorf("retryAfterFrom(%s) = %q, want %q", c.recover.Sub(now), got, c.want)
			}
		})
	}
}

// Hopeless pools have no recovery to advertise. Sending a made-up figure would
// have clients back off against a number nobody measured.
func TestRetryAfterIsZeroWhenHopeless(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{ID: 1, Status: "expired"}}

	outcome, recover := poolAvailability(accs, nil, "GLM-5.3", now, now.Add(10*time.Minute))
	if outcome != poolHopeless {
		t.Fatalf("outcome = %v, want poolHopeless", outcome)
	}
	if got := retryAfterFrom(recover, now); got != "0" {
		t.Errorf("retryAfterFrom on a hopeless pool = %q, want \"0\" (nothing recoverable)", got)
	}
}

// The two exclusion ladders — pickExcept's servable() and recoveryAt's — must
// agree. Both bugs in this ticket came from them drifting: the plan check and
// the model-scoped dead marker were honoured by one and not the other.
//
// Parity, cheaply: an Account that pickExcept refuses is exactly one that
// recoveryAt must not count as a recovery candidate.
func TestRecoveryMatchesPickExceptEligibility(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name string
		acc  store.Account
		// picksNow is what pickExcept would decide at this instant.
		picksNow bool
	}{
		{"healthy", store.Account{ID: 1}, true},
		{"cooling", store.Account{ID: 1, CoolingUntil: now.Add(5 * time.Minute).Unix()}, false},
		{"expired", store.Account{ID: 1, Status: "expired"}, false},
		{"revoked", store.Account{ID: 1, Status: "revoked"}, false},
		{"model excluded", store.Account{ID: 1, DisabledModels: "glm53"}, false},
		{"plan lacks model",
			store.Account{ID: 1, QuotaJSON: `[{"name":"other","limit":10,"remaining":10}]`}, false},
		{"plan has model",
			store.Account{ID: 1, QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":10}]`}, true},
		{"plan has model, quota exhausted",
			store.Account{ID: 1, QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":0}]`}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// poolAvailability on a single account: readyNow means it can serve.
			outcome, _ := poolAvailability([]store.Account{c.acc}, nil, "GLM-5.3", now, now.Add(10*time.Minute))
			servable := outcome == poolReadyNow
			if servable != c.picksNow {
				t.Errorf("poolAvailability says servable=%v, pickExcept would say %v — the ladders disagree",
					servable, c.picksNow)
			}
		})
	}
}

// The regression this ticket introduced lived in the walk loop's use of the
// estimate, not in the estimate itself: "no wait needed" was read as "give up".
// These cases pin the decision the loop makes, which is the pair of questions it
// actually asks.
func TestWalkHoldDecision(t *testing.T) {
	now := time.Now()
	holdUntil := now.Add(10 * time.Minute)

	cases := []struct {
		name string
		accs []store.Account
		// wantHold: should the loop sleep and re-walk instead of answering 529?
		wantHold bool
	}{
		{
			name: "transient failure: every Account still healthy",
			accs: []store.Account{{ID: 1}, {ID: 2}},
			// This is the regression: the pool looks ready after a 429 the egress
			// walk absorbed, and the old code answered immediately.
			wantHold: true,
		},
		{
			name:     "cooling, recovering inside the window",
			accs:     []store.Account{{ID: 1, CoolingUntil: now.Add(3 * time.Minute).Unix()}},
			wantHold: true,
		},
		{
			name:     "cooling beyond the window",
			accs:     []store.Account{{ID: 1, CoolingUntil: now.Add(16 * time.Minute).Unix()}},
			wantHold: false,
		},
		{
			name:     "all expired",
			accs:     []store.Account{{ID: 1, Status: "expired"}},
			wantHold: false,
		},
		{
			name:     "empty pool",
			accs:     nil,
			wantHold: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, _ := poolAvailability(c.accs, nil, "GLM-5.3", now, holdUntil)
			got := outcome != poolHopeless
			if got != c.wantHold {
				t.Errorf("hold=%v, want %v (outcome %v)", got, c.wantHold, outcome)
			}
		})
	}
}

// A model no Account can ever serve is a request error, not a load problem.
// Answering 529 tells the client to back off and retry, which can never succeed
// — the pool will not grow a model it does not have.
func TestUnknownModelIsNotOverload(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{
		ID:        1,
		QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":10}]`,
	}}

	outcome, _ := poolAvailability(accs, nil, "NO-SUCH-MODEL", now, now.Add(10*time.Minute))
	if outcome != poolUnsupportedModel {
		t.Errorf("outcome = %v, want poolUnsupportedModel", outcome)
	}
}

// A pool that is merely exhausted is still an overload: the model exists, there
// is just nothing free right now.
func TestExhaustedPoolIsOverloadNotUnsupported(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{ID: 1, CoolingUntil: now.Add(30 * time.Minute).Unix()}}

	outcome, _ := poolAvailability(accs, nil, "GLM-5.3", now, now.Add(10*time.Minute))
	if outcome != poolHopeless {
		t.Errorf("outcome = %v, want poolHopeless (overload)", outcome)
	}
}

// Pin the exhaustion contract: an exhausted pool answers 529 with a Retry-After
// that reflects a real recovery time, and an unknown model answers 400 rather
// than a retryable 529. These are the two statuses a client branches on.
func TestExhaustedPoolStatusAndRetryAfter(t *testing.T) {
	now := time.Now()
	holdUntil := now.Add(10 * time.Minute)

	// Cooling further out than the hold: hopeless, but the model exists.
	exhausted := []store.Account{{
		ID:           1,
		CoolingUntil: now.Add(30 * time.Minute).Unix(),
		QuotaJSON:    `[{"name":"GLM-5.3","limit":10,"remaining":10}]`,
	}}
	outcome, recover := poolAvailability(exhausted, nil, "GLM-5.3", now, holdUntil)
	if outcome != poolHopeless {
		t.Fatalf("outcome = %v, want poolHopeless (overload)", outcome)
	}
	if got := retryAfterFrom(recover, now); got != "1800" {
		t.Errorf("Retry-After = %q, want 1800 (the 30-minute recovery)", got)
	}

	// A model no plan carries: not an overload, and retrying cannot help.
	unsupported := []store.Account{{
		ID:        1,
		QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":10}]`,
	}}
	if outcome, _ := poolAvailability(unsupported, nil, "NO-SUCH-MODEL", now, holdUntil); outcome != poolUnsupportedModel {
		t.Errorf("outcome = %v, want poolUnsupportedModel so the caller answers 400", outcome)
	}
}

// The dangerous direction is answering 400 for a model that merely cannot be
// served right now: the client reads 400 as its own mistake and never retries,
// while 529 invites one. An Account whose plan was never scanned says nothing
// about which models exist, so it must never produce "unsupported".
func TestUnscannedPlanNeverConcludesUnsupported(t *testing.T) {
	now := time.Now()
	holdUntil := now.Add(10 * time.Minute)

	cases := []struct {
		name string
		accs []store.Account
	}{
		{
			name: "freshly added account, plan not fetched yet",
			accs: []store.Account{{ID: 1}}, // empty QuotaJSON
		},
		{
			name: "account cooling with an unscanned plan",
			accs: []store.Account{{ID: 1, CoolingUntil: now.Add(30 * time.Minute).Unix()}},
		},
		{
			name: "mix: one scanned without the model, one unscanned",
			accs: []store.Account{
				{ID: 1, QuotaJSON: `[{"name":"other","limit":10,"remaining":10}]`},
				{ID: 2},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if outcome, _ := poolAvailability(c.accs, nil, "GLM-5.3", now, holdUntil); outcome == poolUnsupportedModel {
				t.Error("an unscanned plan cannot prove a model unsupported; this would answer 400 for a valid model")
			}
		})
	}
}

// Mixed pools: an Account that does support the model but cannot serve it right
// now must not let the pool be classified as "model not offered". That would be
// overload reported as a permanent client error.
func TestMixedPoolIsOverloadNotUnsupported(t *testing.T) {
	now := time.Now()
	holdUntil := now.Add(10 * time.Minute)

	accs := []store.Account{
		// Supports the model, but expired.
		{ID: 1, Status: "expired",
			QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":10}]`},
		// Active, but its plan does not include this model.
		{ID: 2, QuotaJSON: `[{"name":"other","limit":10,"remaining":10}]`},
		// Supports the model, quota spent.
		{ID: 3, QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":0}]`},
		// Supports the model, cooling well past the hold.
		{ID: 4, CoolingUntil: now.Add(30 * time.Minute).Unix(),
			QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":10}]`},
	}

	outcome, _ := poolAvailability(accs, nil, "GLM-5.3", now, holdUntil)
	if outcome == poolUnsupportedModel {
		t.Error("an expired account supporting the model proves it exists; overload must not read as unsupported")
	}
	if outcome != poolHopeless {
		t.Errorf("outcome = %v, want poolHopeless (overload)", outcome)
	}
}

// pickExcept matches model names through Norm (case and punctuation stripped).
// The unsupported check has to agree, or a valid alias is answered 400.
func TestUnsupportedCheckUsesNormalisedNames(t *testing.T) {
	now := time.Now()
	holdUntil := now.Add(10 * time.Minute)

	accs := []store.Account{{
		ID:        1,
		QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":10}]`,
	}}

	for _, spelling := range []string{"GLM-5.3", "glm-5.3", "glm53", "GLM53"} {
		outcome, _ := poolAvailability(accs, nil, spelling, now, holdUntil)
		if outcome == poolUnsupportedModel {
			t.Errorf("%q answered unsupported, but the plan carries that model under a different spelling", spelling)
		}
	}
}

// Every Account unscanned: the pool knows nothing about which models exist, so
// it must not claim one is unsupported. This exercises the fallback that a
// single unscanned Account would otherwise mask by returning early.
func TestAllUnscannedPlansNeverConcludeUnsupported(t *testing.T) {
	now := time.Now()
	// Each is model-dead so recoveryAt reports nothing waiting, which is the only
	// path that consults the plan at all.
	accs := []store.Account{{ID: 1}, {ID: 2}}
	dead := map[string]int64{
		"1:glm53": now.Add(30 * time.Minute).Unix(),
		"2:glm53": now.Add(40 * time.Minute).Unix(),
	}

	outcome, _ := poolAvailability(accs, dead, "GLM-5.3", now, now.Add(10*time.Minute))
	if outcome == poolUnsupportedModel {
		t.Error("a pool with no scanned plans cannot prove a model unsupported")
	}
}

// An expired Account can carry a scanned plan, so it can testify that the model
// exists. If every Account is expired, that testimony is the only thing standing
// between the client and a 400 for a model the pool really does support.
func TestExpiredAccountsStillTestifyForModel(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{
		ID:        1,
		Status:    "expired",
		QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":10}]`,
	}}

	outcome, _ := poolAvailability(accs, nil, "GLM-5.3", now, now.Add(10*time.Minute))
	if outcome == poolUnsupportedModel {
		t.Error("an expired account's plan proves the model exists; overload must not read as unsupported")
	}
}

// A model the gateway cannot serve is a missing resource, not a malformed
// request: 404 with code "model_not_found", the field OpenAI SDKs branch on.
// It must not invite a retry — there is no Retry-After, because nothing will
// change.
func TestUnsupportedModelResponseContract(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIModelNotFound(rec, "NO-SUCH-MODEL")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After = %q, want absent — this error is permanent", got)
	}

	var out struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	// The type stays invalid_request_error: OpenAI does not constrain it and
	// "code" is the field that distinguishes this error. Inventing a new type
	// would make clients branch on something they never had to before.
	if out.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", out.Error.Type)
	}
	if out.Error.Code == nil || *out.Error.Code != "model_not_found" {
		t.Errorf("code = %v, want model_not_found", out.Error.Code)
	}
	if out.Error.Param == nil || *out.Error.Param != "model" {
		t.Errorf("param = %v, want \"model\"", out.Error.Param)
	}
	if strings.Contains(out.Error.Message, "pool") || strings.Contains(out.Error.Message, "account") {
		t.Errorf("message leaks internal structure: %q", out.Error.Message)
	}
	if !strings.Contains(out.Error.Message, "NO-SUCH-MODEL") {
		t.Errorf("message should name the model, got %q", out.Error.Message)
	}
}

// A hostile or careless client can send an enormous model name; it must not
// inflate the error body.
func TestUnsupportedModelMessageIsBounded(t *testing.T) {
	long := strings.Repeat("x", 5000)
	rec := httptest.NewRecorder()
	writeOpenAIModelNotFound(rec, long)

	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(out.Error.Message) > 200 {
		t.Errorf("message length = %d, want bounded (<=200)", len(out.Error.Message))
	}
	if len(rec.Body.Bytes()) > 1000 {
		t.Errorf("body = %d bytes, want bounded", len(rec.Body.Bytes()))
	}
}

// End-to-end through the real handler: the 404 must come out of the HTTP
// surface, not just out of the helper. This path needs no origin — the verdict
// is decided before anything is forwarded — so it can be tested today, without
// the injectable client that issue #18 will bring.
func TestChatCompletionsReturns404ForUnknownModel(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// One Account whose scanned plan carries only another model.
	acc, err := db.Add("jwt.a", "a")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := db.UpdateQuota(acc.ID, store.Quota{
		Remaining: 10, Limit: 10,
		Entitlements: []map[string]any{
			{"name": "GLM-5.3", "limit": 10, "remaining": 10},
		},
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	srv := &Server{
		store:   db,
		catalog: NewCatalog(nil),
		cfg:     config.Config{GatewayKey: "k", RequestHold: 10 * time.Minute},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"NO-SUCH-MODEL","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()

	srv.handleChatCompletions(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Error struct {
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if out.Error.Code == nil || *out.Error.Code != "model_not_found" {
		t.Errorf("code = %v, want model_not_found", out.Error.Code)
	}
}

// fakeOrigin is the narrow slice of the upstream Client the request walk needs:
// one method. The walk holds the whole Client today, which is why it cannot be
// driven in a test.
type fakeOrigin struct {
	// reply produces a response per attempt; attempts are counted so a test can
	// make the first fail and the next succeed.
	reply func(attempt int) (*http.Response, error)
	calls int
}

func (f *fakeOrigin) PostMessages(ctx context.Context, jwt, deviceMid string, body []byte, extra http.Header) (*http.Response, error) {
	f.calls++
	return f.reply(f.calls)
}

// The walk must hold and re-walk after a transient origin failure, not answer
// 529 on the first refusal. This is the regression #16 introduced, and it lives
// in the loop — the one place the per-function tests cannot reach.
func TestWalkHoldsAndRetriesAfterTransientFailure(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	acc, err := db.Add("jwt.a", "a")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := db.UpdateQuota(acc.ID, store.Quota{
		Remaining: 10, Limit: 10,
		Entitlements: []map[string]any{{"name": "GLM-5.3", "limit": 10, "remaining": 10}},
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	if err := db.Select(acc.ID); err != nil {
		t.Fatalf("select: %v", err)
	}

	sse := strings.Join([]string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":1}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}, "")

	origin := &fakeOrigin{reply: func(attempt int) (*http.Response, error) {
		if attempt == 1 {
			// A transient failure that sets no cooldown and no dead marker —
			// exactly the shape that used to look "servable" to the pool check.
			return nil, errors.New("transient origin failure")
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}}

	srv := &Server{
		store:     db,
		catalog:   NewCatalog(nil),
		origin:    origin,
		stickyAcc: map[string]int64{},
		quota1005: map[string]int{},
		chatOK:    map[int64]time.Time{},
		cfg: config.Config{
			RequestHold:   10 * time.Minute,
			AccountMaxTry: 1,
			HoldTick:      100 * time.Millisecond,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleChatCompletions(rec, req)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("handler never returned")
	}

	if origin.calls < 2 {
		t.Errorf("origin called %d times, want >= 2 (the walk must retry after a transient failure)", origin.calls)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the retry (body=%s)", rec.Code, rec.Body.String())
	}
	// A bare Recorder reports 200 even when nothing was written, so check the
	// body is actually a completion rather than trusting the status alone.
	if !strings.Contains(rec.Body.String(), "chat.completion") {
		t.Errorf("body is not a completion: %q", rec.Body.String())
	}
}

// The opposite branch through the same real handler: a pool that cannot recover
// inside the window must answer at once rather than sitting out the hold.
func TestWalkFailsFastWhenPoolCannotRecover(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	acc, err := db.Add("jwt.a", "a")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := db.UpdateQuota(acc.ID, store.Quota{
		Remaining: 10, Limit: 10,
		Entitlements: []map[string]any{{"name": "GLM-5.3", "limit": 10, "remaining": 10}},
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	// Cooling far past the hold: recovery is real but outside the window.
	db.SetCooling(acc.ID, time.Now().Add(30*time.Minute).Unix())
	if err := db.Select(acc.ID); err != nil {
		t.Fatalf("select: %v", err)
	}

	origin := &fakeOrigin{reply: func(int) (*http.Response, error) {
		t.Error("the Origin must not be called when the pool is hopeless")
		return nil, errors.New("unreachable")
	}}

	srv := &Server{
		store:     db,
		catalog:   NewCatalog(nil),
		origin:    origin,
		stickyAcc: map[string]int64{},
		quota1005: map[string]int{},
		chatOK:    map[int64]time.Time{},
		cfg: config.Config{
			RequestHold:   10 * time.Minute,
			AccountMaxTry: 1,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	start := time.Now()
	srv.handleChatCompletions(rec, req)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("answered after %s; a hopeless pool must not sit out the 10m hold", elapsed.Round(time.Second))
	}
	if rec.Code != 529 {
		t.Errorf("status = %d, want 529", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Errorf("Retry-After = %q, want the real recovery time", got)
	}
	if origin.calls != 0 {
		t.Errorf("origin called %d times, want 0", origin.calls)
	}
}

// Scenario the ticket lists but nothing covered: an Account recovering inside
// the window must be waited for and then served. With a short hold tick this is
// cheap; the walk's sleep is a policy field rather than a bare 15-second literal.
func TestWalkWaitsForRecoveryInsideWindowThenServes(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	acc, err := db.Add("jwt.a", "a")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := db.UpdateQuota(acc.ID, store.Quota{
		Remaining: 10, Limit: 10,
		Entitlements: []map[string]any{{"name": "GLM-5.3", "limit": 10, "remaining": 10}},
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	// Cooling for a brief moment, well inside the hold window.
	db.SetCooling(acc.ID, time.Now().Add(600*time.Millisecond).Unix())
	if err := db.Select(acc.ID); err != nil {
		t.Fatalf("select: %v", err)
	}

	sse := strings.Join([]string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":1}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}, "")

	origin := &fakeOrigin{reply: func(int) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}}

	srv := &Server{
		store:     db,
		catalog:   NewCatalog(nil),
		origin:    origin,
		stickyAcc: map[string]int64{},
		quota1005: map[string]int{},
		chatOK:    map[int64]time.Time{},
		cfg: config.Config{
			RequestHold:   10 * time.Minute,
			AccountMaxTry: 1,
			HoldTick:      100 * time.Millisecond,
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()

	start := time.Now()
	srv.handleChatCompletions(rec, req)
	elapsed := time.Since(start)

	if origin.calls == 0 {
		t.Fatal("the Origin was never called; the walk gave up instead of waiting")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 after the cooldown lapsed (body=%s)", rec.Code, rec.Body.String())
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s; should serve as soon as the cooldown lapses", elapsed.Round(time.Millisecond))
	}
}
