package gateway

import (
	"testing"
	"time"

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
