package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

const (
	buyer = "0.0.1001"
	other = "0.0.2002"
	rate  = int64(3)
)

func newWithProvider(t *testing.T, ttl time.Duration) (*Store, *Provider) {
	t.Helper()
	s := New(ttl)
	p := s.Register(&Provider{AccountID: "0.0.5005", DisplayName: "Test", Capability: "text-generation", RatePerUnit: rate})
	return s, p
}

// completed dispatches a job and completes it, which is the state a collect starts from.
func completed(t *testing.T, s *Store, pid string, maxUnits, reported int64) *Job {
	t.Helper()
	j, err := s.CreateJob(pid, buyer, "prompt", maxUnits)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	done, err := s.Complete(j.ID, reported, "the work product", rate)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return done
}

func TestRegisterAssignsIDAndKeepsNoPrivateKey(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	if p.ID == "" {
		t.Fatal("no provider id assigned")
	}
	got, err := s.Provider(p.ID)
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	if got.AccountID != "0.0.5005" {
		t.Errorf("account = %q", got.AccountID)
	}
	if got.Online {
		t.Error("a freshly registered provider should not be online until it dials in")
	}
}

// TestProviderReturnsACopy matters because handlers mutate what they get back. Handing out the
// live pointer would let a request corrupt shared state without holding the lock.
func TestProviderReturnsACopy(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	got, _ := s.Provider(p.ID)
	got.RatePerUnit = 9999
	again, _ := s.Provider(p.ID)
	if again.RatePerUnit != rate {
		t.Errorf("mutating a returned provider changed the store: rate = %d", again.RatePerUnit)
	}
}

func TestJobReturnsACopy(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	j := completed(t, s, p.ID, 100, 10)
	got, _ := s.Job(j.ID, buyer)
	got.Price = 12345
	again, _ := s.Job(j.ID, buyer)
	if again.Price != j.Price {
		t.Errorf("mutating a returned job changed the store: price = %d", again.Price)
	}
}

func TestProvidersFilter(t *testing.T) {
	s := New(time.Minute)
	cheap := s.Register(&Provider{AccountID: "0.0.1", Capability: "text-generation", RatePerUnit: 1})
	s.Register(&Provider{AccountID: "0.0.2", Capability: "text-generation", RatePerUnit: 100})
	s.Register(&Provider{AccountID: "0.0.3", Capability: "image", RatePerUnit: 1})
	s.SetOnline(cheap.ID, true)

	if got := len(s.Providers("", 0, false)); got != 3 {
		t.Errorf("unfiltered = %d, want 3", got)
	}
	if got := len(s.Providers("text-generation", 0, false)); got != 2 {
		t.Errorf("by capability = %d, want 2", got)
	}
	if got := s.Providers("", 50, false); len(got) != 2 {
		t.Errorf("by max rate = %d, want 2 (the 100 excluded)", len(got))
	}
	if got := s.Providers("", 0, true); len(got) != 1 || got[0].ID != cheap.ID {
		t.Errorf("online only = %+v, want just the connected one", got)
	}
}

func TestSetOnlineTracksConnection(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	s.SetOnline(p.ID, true)
	if got, _ := s.Provider(p.ID); !got.Online {
		t.Error("not marked online")
	}
	s.SetOnline(p.ID, false)
	if got, _ := s.Provider(p.ID); got.Online {
		t.Error("not marked offline")
	}
	s.SetOnline("prov-does-not-exist", true) // must not panic
}

func TestCreateJobRejectsUnknownProvider(t *testing.T) {
	s := New(time.Minute)
	if _, err := s.CreateJob("prov-nope", buyer, "hi", 10); !errors.Is(err, ErrNoSuchProvider) {
		t.Errorf("err = %v, want ErrNoSuchProvider", err)
	}
}

func TestJobIDsAreUnguessable(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	seen := map[string]bool{}
	for range 100 {
		j, err := s.CreateJob(p.ID, buyer, "hi", 10)
		if err != nil {
			t.Fatal(err)
		}
		if seen[j.ID] {
			t.Fatalf("duplicate job id %s", j.ID)
		}
		seen[j.ID] = true
		if len(j.ID) < 16 {
			t.Fatalf("job id too short to be unguessable: %q", j.ID)
		}
	}
}

// TestOwnershipIsEnforced guards the one thing that stops a caller collecting a result it did not
// commission. Ids are unguessable, but that is not the control.
func TestOwnershipIsEnforced(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	j := completed(t, s, p.ID, 100, 10)

	if _, err := s.Job(j.ID, other); !errors.Is(err, ErrNotYours) {
		t.Errorf("another buyer got the job: err = %v", err)
	}
	if _, err := s.Job(j.ID, buyer); err != nil {
		t.Errorf("the owner was refused: %v", err)
	}
	// An empty buyer means "do not check" — used by internal paths that already know the owner.
	if _, err := s.Job(j.ID, ""); err != nil {
		t.Errorf("internal lookup refused: %v", err)
	}
	if _, err := s.Job("job-nope", buyer); !errors.Is(err, ErrNoSuchJob) {
		t.Errorf("err = %v, want ErrNoSuchJob", err)
	}
}

func TestLifecycle(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	j, _ := s.CreateJob(p.ID, buyer, "hi", 100)
	if j.State != JobDispatched {
		t.Fatalf("state = %q", j.State)
	}
	if j.Billable() {
		t.Error("a dispatched job is billable")
	}

	s.MarkRunning(j.ID)
	if got, _ := s.Job(j.ID, buyer); got.State != JobRunning {
		t.Errorf("state = %q, want running", got.State)
	}

	done, err := s.Complete(j.ID, 10, "result", rate)
	if err != nil {
		t.Fatal(err)
	}
	if !done.Billable() {
		t.Error("a completed job is not billable")
	}

	s.MarkCollected(j.ID, "0.0.7162784@123.456")
	got, _ := s.Job(j.ID, buyer)
	if got.State != JobCollected || got.TxID == "" {
		t.Errorf("after collect: %+v", got)
	}
	if got.Billable() {
		t.Error("an already-collected job is still billable — it would be charged twice")
	}
}

// TestClampsToBuyerCeiling is the metering defence. The provider counts the units it bills for, so
// an inflated report must cost the buyer nothing beyond the ceiling it declared up front.
func TestClampsToBuyerCeiling(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		maxUnits, reported     int64
		wantPriced, wantCharge int64
	}{
		{"under the ceiling", 100, 10, 10, 30},
		{"exactly at it", 100, 100, 100, 300},
		{"wildly inflated", 100, 1_000_000, 100, 300},
		{"negative, treated as zero", 100, -5, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, p := newWithProvider(t, time.Minute)
			j := completed(t, s, p.ID, tc.maxUnits, tc.reported)
			if j.Priced != tc.wantPriced {
				t.Errorf("priced = %d, want %d", j.Priced, tc.wantPriced)
			}
			if j.Price != tc.wantCharge {
				t.Errorf("price = %d tinybar, want %d", j.Price, tc.wantCharge)
			}
			// The over-report is kept, not discarded: the gap between reported and priced is the
			// only cross-call evidence of systematic inflation.
			if j.Reported != tc.reported {
				t.Errorf("reported = %d, want the provider's raw claim %d", j.Reported, tc.reported)
			}
		})
	}
}

func TestFailedJobIsNeverBillable(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	j, _ := s.CreateJob(p.ID, buyer, "hi", 100)
	s.Fail(j.ID, "the backend exploded")

	got, _ := s.Job(j.ID, buyer)
	if got.State != JobFailed {
		t.Fatalf("state = %q", got.State)
	}
	if got.Billable() {
		t.Error("a failed job is billable — the buyer would be charged for nothing")
	}
	if got.Price != 0 {
		t.Errorf("price = %d, want 0", got.Price)
	}
	if got.Error == "" {
		t.Error("failure reason not retained")
	}
}

func TestTerminalStatesAreFinal(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)

	j := completed(t, s, p.ID, 100, 10)
	s.MarkCollected(j.ID, "tx-1")
	if _, err := s.Complete(j.ID, 999, "different", rate); err == nil {
		t.Error("re-completing a collected job was allowed — it would reprice paid work")
	}

	k, _ := s.CreateJob(p.ID, buyer, "hi", 100)
	s.Fail(k.ID, "boom")
	s.Fail(k.ID, "boom again")
	if got, _ := s.Job(k.ID, buyer); got.Error != "boom" {
		t.Errorf("a second failure overwrote the first: %q", got.Error)
	}
	if _, err := s.Complete(k.ID, 10, "result", rate); err == nil {
		t.Error("completing a failed job was allowed")
	}
}

func TestCompleteUnknownJob(t *testing.T) {
	s := New(time.Minute)
	if _, err := s.Complete("job-nope", 1, "x", rate); !errors.Is(err, ErrNoSuchJob) {
		t.Errorf("err = %v, want ErrNoSuchJob", err)
	}
}

// TestSweepExpiresUncollected covers the held-result TTL. A completed job nobody pays for must not
// pin its result forever, and expiry has to be distinguishable from failure.
func TestSweepExpiresUncollected(t *testing.T) {
	s, p := newWithProvider(t, time.Nanosecond) // everything is already past its TTL
	j := completed(t, s, p.ID, 100, 10)
	time.Sleep(2 * time.Millisecond)

	if n := s.Sweep(); n != 1 {
		t.Fatalf("swept %d, want 1", n)
	}
	got, _ := s.Job(j.ID, buyer)
	if got.State != JobExpired {
		t.Errorf("state = %q, want expired", got.State)
	}
	if got.Result != "" {
		t.Error("the result was not released on expiry")
	}
	if got.Billable() {
		t.Error("an expired job is billable")
	}
	if n := s.Sweep(); n != 0 {
		t.Errorf("second sweep touched %d jobs; expiry is terminal", n)
	}
}

func TestSweepLeavesCollectedAlone(t *testing.T) {
	s, p := newWithProvider(t, time.Nanosecond)
	j := completed(t, s, p.ID, 100, 10)
	s.MarkCollected(j.ID, "tx-1")
	time.Sleep(2 * time.Millisecond)

	if n := s.Sweep(); n != 0 {
		t.Errorf("swept %d paid jobs; a buyer who paid keeps the result", n)
	}
	if got, _ := s.Job(j.ID, buyer); got.Result == "" {
		t.Error("a paid result was released")
	}
}

// TestBuyerStats feeds the budget, velocity and abandonment rules. Only settled jobs count toward
// spend: dispatch is free, so an uncollected job costs the buyer nothing and must not.
func TestBuyerStats(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)

	paid := completed(t, s, p.ID, 100, 10) // 30 tinybar
	s.MarkCollected(paid.ID, "tx-1")
	completed(t, s, p.ID, 100, 20)                // completed, never collected
	unpaid := completed(t, s, p.ID, 100, 5)       // will be expired below
	s.CreateJob(p.ID, other, "someone else", 100) // different buyer, must not count

	spend, calls, abandoned, comp := s.BuyerStats(buyer, time.Hour)
	if spend != 30 {
		t.Errorf("spend = %d, want 30 (only the settled job)", spend)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 for this buyer", calls)
	}
	if abandoned != 0 {
		t.Errorf("abandoned = %d before any expiry", abandoned)
	}
	if comp != 1 {
		t.Errorf("completed = %d, want 1", comp)
	}

	// Expiring an uncollected job is what abandonment means.
	s.mu.Lock()
	s.jobs[unpaid.ID].State = JobExpired
	s.mu.Unlock()
	_, _, abandoned, comp = s.BuyerStats(buyer, time.Hour)
	if abandoned != 1 || comp != 2 {
		t.Errorf("abandoned = %d, completed = %d, want 1 and 2", abandoned, comp)
	}
}

func TestBuyerStatsWindowExcludesOldCalls(t *testing.T) {
	s, p := newWithProvider(t, time.Hour)
	j := completed(t, s, p.ID, 100, 10)
	s.MarkCollected(j.ID, "tx-1")

	if _, calls, _, _ := s.BuyerStats(buyer, time.Hour); calls != 1 {
		t.Errorf("calls in a wide window = %d, want 1", calls)
	}
	if _, calls, _, _ := s.BuyerStats(buyer, time.Nanosecond); calls != 0 {
		t.Errorf("calls in a closed window = %d, want 0", calls)
	}
}

func TestSpendWith(t *testing.T) {
	s := New(time.Minute)
	a := s.Register(&Provider{AccountID: "0.0.1", RatePerUnit: rate})
	b := s.Register(&Provider{AccountID: "0.0.2", RatePerUnit: rate})

	for _, pid := range []string{a.ID, a.ID, b.ID} {
		j := completed(t, s, pid, 100, 10)
		s.MarkCollected(j.ID, "tx")
	}
	completed(t, s, a.ID, 100, 10) // uncollected: not spend

	spend, calls := s.SpendWith(buyer, a.ID)
	if spend != 60 || calls != 2 {
		t.Errorf("with a: spend = %d calls = %d, want 60 and 2", spend, calls)
	}
	if spend, calls := s.SpendWith(buyer, b.ID); spend != 30 || calls != 1 {
		t.Errorf("with b: spend = %d calls = %d, want 30 and 1", spend, calls)
	}
	if spend, _ := s.SpendWith(other, a.ID); spend != 0 {
		t.Errorf("another buyer's spend leaked: %d", spend)
	}
}

// TestConcurrentAccess is a race-detector target: every handler touches this store from its own
// goroutine, so run it with -race.
func TestConcurrentAccess(t *testing.T) {
	s, p := newWithProvider(t, time.Minute)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j, err := s.CreateJob(p.ID, buyer, "hi", 100)
			if err != nil {
				t.Error(err)
				return
			}
			s.MarkRunning(j.ID)
			if _, err := s.Complete(j.ID, int64(i), "r", rate); err != nil {
				t.Error(err)
				return
			}
			s.MarkCollected(j.ID, "tx")
			s.BuyerStats(buyer, time.Hour)
			s.SpendWith(buyer, p.ID)
			s.Providers("", 0, false)
			s.SetOnline(p.ID, i%2 == 0)
		}(i)
	}
	wg.Wait()
	if _, calls, _, _ := s.BuyerStats(buyer, time.Hour); calls != 20 {
		t.Errorf("calls = %d, want 20", calls)
	}
}
