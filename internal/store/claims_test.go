package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClaimFreePathAndReclaimUpdatesNote(t *testing.T) {
	s, nA, _ := twoAgents(t)
	res, err := s.Claim("/r", nA, "  internal/hub.go  ", "rewiring dispatch")
	if err != nil || !res.Granted || res.Stolen {
		t.Fatalf("free claim: %+v err=%v", res, err)
	}
	res, err = s.Claim("/r", nA, "internal/hub.go", "still rewiring")
	if err != nil || !res.Granted || res.Stolen {
		t.Fatalf("re-claim by holder must be idempotent: %+v err=%v", res, err)
	}
	claims, err := s.ListClaims("/r")
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims: %+v err=%v", claims, err)
	}
	if claims[0].Path != "internal/hub.go" || claims[0].Note != "still rewiring" {
		t.Fatalf("re-claim must trim the path and update the note: %+v", claims[0])
	}
}

func TestClaimHeldByLivePeerConflicts(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if _, err := s.Claim("/r", nA, "/hub.go", "rewiring"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Claim("/r", nB, "/hub.go", "mine")
	var held *ErrClaimHeld
	if !errors.As(err, &held) {
		t.Fatalf("want ErrClaimHeld, got %v", err)
	}
	if held.Holder != nA || held.HolderID == "" || held.Note != "rewiring" || held.Since == 0 {
		t.Fatalf("conflict details wrong: %+v", held)
	}
	if msg := err.Error(); !strings.Contains(msg, "held by "+nA+" ("+held.HolderID+"): rewiring") {
		t.Fatalf("conflict message wrong: %q", msg)
	}
	// The failed claim must not change the holder.
	claims, _ := s.ListClaims("/r")
	if len(claims) != 1 || claims[0].Holder != nA || claims[0].Note != "rewiring" {
		t.Fatalf("holder changed on conflict: %+v", claims)
	}
}

func TestClaimStealsFromGoneHolder(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if _, err := s.Claim("/r", nA, "/hub.go", "rewiring"); err != nil {
		t.Fatal(err)
	}
	s.SetStatus("/r", "sess-a", "gone")
	res, err := s.Claim("/r", nB, "/hub.go", "taking over")
	if err != nil || !res.Granted || !res.Stolen || res.PrevName != nA {
		t.Fatalf("claim from gone holder must steal and report takeover: %+v err=%v", res, err)
	}
	claims, _ := s.ListClaims("/r")
	if len(claims) != 1 || claims[0].Holder != nB || claims[0].Note != "taking over" {
		t.Fatalf("takeover not recorded: %+v", claims)
	}
}

func TestClaimStealsFromDecayedHolder(t *testing.T) {
	s := open(t)
	now := time.Unix(1000000, 0)
	s.Now = func() time.Time { return now }
	nA, _ := s.Register("/r", "sess-a", "startup")
	if _, err := s.Claim("/r", nA, "/hub.go", "rewiring"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour) // freshStatus decays sess-a to gone
	nB, _ := s.Register("/r", "sess-b", "startup")
	res, err := s.Claim("/r", nB, "/hub.go", "mine now")
	if err != nil || !res.Stolen || res.PrevName != nA {
		t.Fatalf("decayed-gone holder must be stealable: %+v err=%v", res, err)
	}
}

func TestReleaseRules(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if _, err := s.Claim("/r", nA, "/hub.go", "rewiring"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release("/r", nB, "/hub.go"); err == nil || !strings.Contains(err.Error(), nA) {
		t.Fatalf("non-holder release must be refused naming the holder, got %v", err)
	}
	if err := s.Release("/r", nA, "/hub.go"); err != nil {
		t.Fatalf("holder release: %v", err)
	}
	if claims, _ := s.ListClaims("/r"); len(claims) != 0 {
		t.Fatalf("release did not free: %+v", claims)
	}
	// The freed path is claimable by the other agent now.
	if res, err := s.Claim("/r", nB, "/hub.go", ""); err != nil || !res.Granted || res.Stolen {
		t.Fatalf("claim after release: %+v err=%v", res, err)
	}
	if err := s.Release("/r", nA, "/never-claimed.go"); err != nil {
		t.Fatalf("releasing an unheld path must be a no-op success: %v", err)
	}
}

func TestHousekeepPurgesOrphanClaims(t *testing.T) {
	s := open(t)
	now := time.Unix(3000000, 0)
	s.Now = func() time.Time { return now }
	nOld, _ := s.Register("/r", "s-old", "startup") // purged at sweep (3h stale)
	s.Claim("/r", nOld, "/old.go", "")
	now = now.Add(2 * time.Hour)
	nGone, _ := s.Register("/r", "s-gone", "startup") // row survives, but explicit gone
	s.Claim("/r", nGone, "/gone.go", "")
	s.SetStatus("/r", "s-gone", "gone")
	now = now.Add(time.Hour)
	nLive, _ := s.Register("/r", "s-live", "startup")
	s.Claim("/r", nLive, "/live.go", "working")
	if err := s.Housekeep(); err != nil {
		t.Fatal(err)
	}
	claims, err := s.ListClaims("/r")
	if err != nil || len(claims) != 1 || claims[0].Path != "/live.go" || claims[0].Holder != nLive {
		t.Fatalf("only the live holder's claim may survive housekeep: %+v err=%v", claims, err)
	}
}

func TestListClaimsAndBoardShowClaims(t *testing.T) {
	s, nA, _ := twoAgents(t)
	s.Claim("/r", nA, "docs/plan.md", "drafting")
	s.Claim("/r", nA, "internal/hub.go", "rewiring")
	claims, err := s.ListClaims("/r")
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims: %+v err=%v", claims, err)
	}
	for _, c := range claims {
		if c.Holder != nA || c.HolderID == "" || c.Since == 0 {
			t.Fatalf("claim row must resolve holder name+id: %+v", c)
		}
	}
	board, err := s.Board("/r", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range board {
		if a.Name == nA {
			if len(a.Claims) != 2 || a.Claims[0] != "docs/plan.md" || a.Claims[1] != "internal/hub.go" {
				t.Fatalf("board row must list the agent's claims: %+v", a.Claims)
			}
			return
		}
	}
	t.Fatalf("agent %s missing from board", nA)
}

// Two claimants observing the same gone holder: the conditional takeover lets
// exactly one win; the loser re-runs and hits the winner's live claim.
func TestClaimTakeoverRaceHasOneWinner(t *testing.T) {
	s, nA, nB := twoAgents(t)
	if _, err := s.Claim("/r", nA, "/hub.go", "old"); err != nil {
		t.Fatal(err)
	}
	s.SetStatus("/r", "sess-a", "gone")
	nC, _ := s.Register("/r", "sess-c", "startup")
	// Between B observing the gone holder and stealing, rival C steals first.
	fired := false
	claimRaceHook = func() {
		if fired {
			return
		}
		fired = true
		if _, err := s.db.Exec(`UPDATE claims SET agent_id=? WHERE scope='/r' AND path='/hub.go'`,
			agentID("sess-c")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { claimRaceHook = nil })
	_, err := s.Claim("/r", nB, "/hub.go", "mine")
	var held *ErrClaimHeld
	if !errors.As(err, &held) || held.Holder != nC {
		t.Fatalf("loser must see the winner's claim, got %v", err)
	}
	claims, _ := s.ListClaims("/r")
	if len(claims) != 1 || claims[0].Holder != nC {
		t.Fatalf("exactly one winner: %+v", claims)
	}
}

// Persistent adversarial contention exhausts the bounded retry instead of
// recursing forever.
func TestClaimBoundedRetryUnderContention(t *testing.T) {
	s, _, nB := twoAgents(t)
	// The rival flips the row on every observation: absent rows appear (insert
	// race), present rows vanish (takeover race).
	claimRaceHook = func() {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM claims WHERE scope='/r' AND path='/hub.go'`).Scan(&n)
		if n == 0 {
			s.db.Exec(`INSERT INTO claims (scope, path, agent_id, note, since) VALUES ('/r','/hub.go','ghost','',1)`)
		} else {
			s.db.Exec(`DELETE FROM claims WHERE scope='/r' AND path='/hub.go'`)
		}
	}
	t.Cleanup(func() { claimRaceHook = nil })
	_, err := s.Claim("/r", nB, "/hub.go", "mine")
	if err == nil || !strings.Contains(err.Error(), "lost repeated races") {
		t.Fatalf("want bounded-retry conflict, got %v", err)
	}
}
