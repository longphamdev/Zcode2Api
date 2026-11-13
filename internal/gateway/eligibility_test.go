package gateway

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

// A single decision answers both questions the pool asks: can this Account serve
// this model now, and if not, why — and when, if the reason is temporary.
func TestAccountEligibilityReasons(t *testing.T) {
	now := time.Now()
	nowUnix := now.Unix()
	coolingUntil := now.Add(5 * time.Minute).Unix()
	// The quota helpers read the wall clock for period_end, so the fixture is
	// anchored to it as well.
	periodEnd := time.Now().Add(48 * time.Hour).Unix()
	spentUntil := func(pe int64) string {
		return fmt.Sprintf(`[{"name":"GLM-5.3","limit":10,"remaining":0,"period_end":%d}]`, pe)
	}

	cases := []struct {
		name   string
		acc    store.Account
		dead   map[string]int64
		want   eligibilityReason
		wantAt int64 // expected "until" for temporary reasons; 0 = none
	}{
		{
			name: "healthy",
			acc:  store.Account{ID: 1},
			want: eligibleNow,
		},
		{
			name: "expired",
			acc:  store.Account{ID: 1, Status: "expired"},
			want: ineligibleForever,
		},
		{
			name: "revoked",
			acc:  store.Account{ID: 1, Status: "revoked"},
			want: ineligibleForever,
		},
		{
			name: "model excluded",
			acc:  store.Account{ID: 1, DisabledModels: "glm53"},
			want: ineligibleForever,
		},
		{
			name: "plan lacks model",
			acc:  store.Account{ID: 1, QuotaJSON: `[{"name":"other","limit":10,"remaining":10}]`},
			want: ineligibleForever,
		},
		{
			// Distinct from ineligibleForever: this reason also benches the Account.
			name:   "quota exhausted",
			acc:    store.Account{ID: 1, QuotaJSON: spentUntil(periodEnd)},
			want:   ineligibleQuotaSpent,
			wantAt: periodEnd,
		},
		{
			name:   "cooling",
			acc:    store.Account{ID: 1, CoolingUntil: coolingUntil},
			want:   ineligibleUntil,
			wantAt: coolingUntil,
		},
		{
			name:   "model-dead marker",
			acc:    store.Account{ID: 1},
			dead:   map[string]int64{"1:glm53": nowUnix + 600},
			want:   ineligibleUntil,
			wantAt: nowUnix + 600,
		},
		{
			// Blockers stack: the Account is unusable until the LAST one lapses.
			// Reporting the marker alone told the walk "back in ten minutes" for an
			// Account whose cooldown ran another twenty.
			name:   "marker and cooling: the later one wins",
			acc:    store.Account{ID: 1, CoolingUntil: now.Add(30 * time.Minute).Unix()},
			dead:   map[string]int64{"1:glm53": nowUnix + 600},
			want:   ineligibleUntil,
			wantAt: now.Add(30 * time.Minute).Unix(),
		},
		{
			name:   "cooling and marker: the later one wins either way",
			acc:    store.Account{ID: 1, CoolingUntil: coolingUntil},
			dead:   map[string]int64{"1:glm53": now.Add(30 * time.Minute).Unix()},
			want:   ineligibleUntil,
			wantAt: now.Add(30 * time.Minute).Unix(),
		},
		{
			// Spent quota under a cooldown: the reason stays ineligibleUntil so the
			// caller does not bench, but the wait runs to the quota reset, not to
			// the end of the cooldown — the Account is still empty then.
			name:   "cooling and spent quota: wait for the reset",
			acc:    store.Account{ID: 1, CoolingUntil: coolingUntil, QuotaJSON: spentUntil(periodEnd)},
			want:   ineligibleUntil,
			wantAt: periodEnd,
		},
		{
			// A permanent exclusion beats a temporary one: cooling says nothing
			// about an Account that will never serve this model.
			name: "cooling with a plan that lacks the model",
			acc:  store.Account{ID: 1, CoolingUntil: coolingUntil, QuotaJSON: `[{"name":"other","limit":10,"remaining":10}]`},
			want: ineligibleForever,
		}, {
			name: "plan has model, quota remains",
			acc:  store.Account{ID: 1, QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":3}]`},
			want: eligibleNow,
		},
		{
			name: "plan never scanned",
			acc:  store.Account{ID: 1},
			want: eligibleNow,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := accountEligibility(c.acc, "GLM-5.3", c.dead, now)
			if got.reason != c.want {
				t.Fatalf("reason = %v, want %v", got.reason, c.want)
			}
			if c.wantAt != 0 && got.until.Unix() != c.wantAt {
				t.Errorf("until = %d, want %d", got.until.Unix(), c.wantAt)
			}
		})
	}
}

// A model-dead marker for a different model says nothing about this one.
func TestAccountEligibilityIgnoresOtherModelsMarkers(t *testing.T) {
	now := time.Now()
	dead := map[string]int64{"1:glm53flash": now.Add(10 * time.Minute).Unix()}

	if got := accountEligibility(store.Account{ID: 1}, "GLM-5.3", dead, now); got.reason != eligibleNow {
		t.Errorf("reason = %v, want eligibleNow; another model's marker is not this one's", got.reason)
	}
}

// Refusing an Account for spent quota must also bench it, so the next walk does
// not re-ask the Origin about an Account known to be empty. That side effect
// belongs to the selection path, not to the pure eligibility decision — which is
// exactly why it is easy to drop during a refactor like this one.
func TestPickExceptBenchesExhaustedQuota(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	acc, err := db.Add("jwt.a", "a")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	periodEnd := time.Now().Add(24 * time.Hour).Unix()
	if err := db.UpdateQuota(acc.ID, store.Quota{
		Remaining: 0, Limit: 10,
		Entitlements: []map[string]any{{
			"name": "GLM-5.3", "limit": 10, "remaining": 0, "period_end": periodEnd,
		}},
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	srv := &Server{
		store:     db,
		catalog:   NewCatalog(nil),
		stickyAcc: map[string]int64{},
		quota1005: map[string]int{},
		chatOK:    map[int64]time.Time{},
		cfg:       config.Config{RequestHold: time.Minute, AccountMaxTry: 1},
	}

	if picked, _ := srv.pickExcept("GLM-5.3", nil); picked != nil {
		t.Fatalf("picked an exhausted Account: %+v", picked)
	}
	if dead := db.DeadSet(); !dead["1:glm53"] {
		t.Errorf("the exhausted Account was not benched; dead set = %v", dead)
	}
	if got := db.DeadUntil()["1:glm53"]; got != periodEnd {
		t.Errorf("bench expiry = %d, want the quota period end %d", got, periodEnd)
	}
}

// Benching must fire only for spent quota — not for an Account that is already
// excluded for another reason. Ordering matters: if the quota arm is evaluated
// before cooling or a live marker, an Account that was merely cooling gets
// benched, and an Account with a live marker has that marker's expiry
// overwritten. Neither happened before the eligibility ladder was unified.
func TestPickExceptDoesNotBenchForOtherReasons(t *testing.T) {
	setup := func(t *testing.T, mutate func(db *store.Store, id int64)) (*Server, *store.Store, int64) {
		t.Helper()
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
			Remaining: 0, Limit: 10,
			Entitlements: []map[string]any{{
				"name": "GLM-5.3", "limit": 10, "remaining": 0,
				"period_end": time.Now().Add(24 * time.Hour).Unix(),
			}},
		}); err != nil {
			t.Fatalf("set quota: %v", err)
		}
		mutate(db, acc.ID)
		srv := &Server{
			store:     db,
			catalog:   NewCatalog(nil),
			stickyAcc: map[string]int64{},
			quota1005: map[string]int{},
			chatOK:    map[int64]time.Time{},
			cfg:       config.Config{RequestHold: time.Minute, AccountMaxTry: 1},
		}
		return srv, db, acc.ID
	}

	t.Run("cooling with spent quota is not benched", func(t *testing.T) {
		srv, db, _ := setup(t, func(db *store.Store, id int64) {
			db.SetCooling(id, time.Now().Add(10*time.Minute).Unix())
		})
		if picked, _ := srv.pickExcept("GLM-5.3", nil); picked != nil {
			t.Fatalf("picked a cooling Account")
		}
		if dead := db.DeadSet(); len(dead) != 0 {
			t.Errorf("a cooling Account was benched; dead set = %v — the pre-existing marker or its absence must be untouched", dead)
		}
	})

	t.Run("live marker with spent quota keeps its own expiry", func(t *testing.T) {
		var wantUntil int64
		srv, db, id := setup(t, func(db *store.Store, id int64) {
			// A marker far out; the quota period resets much sooner.
			wantUntil = time.Now().Add(30 * 24 * time.Hour).Unix()
			db.MarkDead(id, "glm53", wantUntil)
		})
		if picked, _ := srv.pickExcept("GLM-5.3", nil); picked != nil {
			t.Fatalf("picked an Account with a live marker")
		}
		got := db.DeadUntil()[fmt.Sprintf("%d:glm53", id)]
		if got != wantUntil {
			t.Errorf("marker expiry = %d, want %d — benching overwrote a live marker", got, wantUntil)
		}
	})
}
