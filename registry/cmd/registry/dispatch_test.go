package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hub"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/store"
)

// connectProvider registers a provider and dials it in over the real websocket route, so dispatch
// runs against the actual hub rather than a stub. reply decides what the provider answers; a nil
// return means it stays silent.
func connectProvider(t *testing.T, s *server, rate int64, reply func(hub.Frame) *hub.Frame) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(s.routes())
	pid := register(t, s, "0.0.5005", rate)

	ws, _, err := websocket.Dial(t.Context(), "ws"+srv.URL[4:]+"/connect?provider_id="+pid, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	go func() {
		for {
			var f hub.Frame
			if err := wsjson.Read(context.Background(), ws, &f); err != nil {
				return
			}
			if out := reply(f); out != nil {
				if err := wsjson.Write(context.Background(), ws, *out); err != nil {
					return
				}
			}
		}
	}()

	for range 200 {
		if s.hub.Online(pid) {
			return pid, func() { ws.CloseNow(); srv.Close() }
		}
		time.Sleep(2 * time.Millisecond)
	}
	ws.CloseNow()
	srv.Close()
	t.Fatal("provider never came online")
	return "", func() {}
}

func answerWith(result string, units int64) func(hub.Frame) *hub.Frame {
	return func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		return &hub.Frame{Type: hub.FrameResult, JobID: f.JobID, Result: result, Units: units}
	}
}

func dispatch(t *testing.T, s *server, pid string, body any, buyer string) *httptest.ResponseRecorder {
	t.Helper()
	h := map[string]string{}
	if buyer != "" {
		h[buyerHeader] = buyer
	}
	return do(t, s, "POST", "/p/"+pid+"/job", body, h)
}

func TestDispatchRunsTheJobAndPricesIt(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, answerWith("the work product", 10))
	defer done()

	w := dispatch(t, s, pid, map[string]any{"prompt": "summarise this", "max_units": 100}, testBuyer)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", w.Code, w.Body.String())
	}
	b := decodeBody(t, w)

	if b["billable"] != true {
		t.Errorf("billable = %v, want true", b["billable"])
	}
	if b["state"] != string(store.JobCompleted) {
		t.Errorf("state = %v", b["state"])
	}
	if b["priced_units"] != float64(10) || b["price_tinybar"] != float64(30) {
		t.Errorf("pricing = %v units / %v tinybar, want 10 and 30", b["priced_units"], b["price_tinybar"])
	}
	// The caller needs to be told where to pay; making it construct the path is a second place for
	// the contract to drift.
	if got, _ := b["collect"].(string); !strings.Contains(got, b["job_id"].(string)) {
		t.Errorf("collect = %q, want it to carry the job id", got)
	}
	// The result is not in the dispatch response: that is what payment buys.
	if strings.Contains(w.Body.String(), "the work product") {
		t.Errorf("dispatch handed over the result for free: %s", w.Body.String())
	}
}

// TestDispatchClampsAnInflatedReport is the metering defence at the HTTP layer. The provider counts
// the units it bills for, so an inflated claim must cost the buyer nothing above its ceiling — and
// the raw claim must still be visible, since the aggregate is the only evidence of inflation.
func TestDispatchClampsAnInflatedReport(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, answerWith("result", 1_000_000))
	defer done()

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 50}, testBuyer)
	b := decodeBody(t, w)

	if b["reported_units"] != float64(1_000_000) {
		t.Errorf("reported = %v, want the provider's raw claim retained", b["reported_units"])
	}
	if b["priced_units"] != float64(50) {
		t.Errorf("priced = %v, want it clamped to the buyer's ceiling of 50", b["priced_units"])
	}
	if b["price_tinybar"] != float64(150) {
		t.Errorf("price = %v tinybar, want 150", b["price_tinybar"])
	}
}

// TestFailedJobIsFree is the invariant that keeps payment delivery-conditional. The work no longer
// sits between verify and settle, so this is what replaces that guarantee.
func TestFailedJobIsFree(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		return &hub.Frame{Type: hub.FrameFailed, JobID: f.JobID, Error: "the backend exploded"}
	})
	defer done()

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 100}, testBuyer)
	// 200, not an HTTP error: the exchange worked, the provider did not. The caller's move is to
	// try another provider, and it needs the job id to say what happened.
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 with billable:false", w.Code)
	}
	b := decodeBody(t, w)
	if b["billable"] != false {
		t.Errorf("billable = %v, want false", b["billable"])
	}
	if b["state"] != string(store.JobFailed) {
		t.Errorf("state = %v", b["state"])
	}
	reason, _ := b["error"].(string)
	if !strings.HasPrefix(reason, policy.PrefixJobFailed) {
		t.Errorf("error = %q, want the %s class so a caller can branch on it", reason, policy.PrefixJobFailed)
	}
	if !strings.Contains(reason, "the backend exploded") {
		t.Errorf("the provider's own reason was dropped: %q", reason)
	}

	// And the job must not be collectable: nothing to sell.
	jobID := b["job_id"].(string)
	j, err := s.store.Job(jobID, testBuyer)
	if err != nil {
		t.Fatal(err)
	}
	if j.Billable() || j.Price != 0 {
		t.Errorf("a failed job is billable at %d tinybar", j.Price)
	}
}

func TestDispatchRequiresBuyerHeader(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, answerWith("r", 1))
	defer done()

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 10}, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestDispatchValidation(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, answerWith("r", 1))
	defer done()

	for _, tc := range []struct {
		name string
		body any
	}{
		{"no prompt", map[string]any{"max_units": 10}},
		{"zero max_units", map[string]any{"prompt": "x", "max_units": 0}},
		{"negative max_units", map[string]any{"prompt": "x", "max_units": -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := dispatch(t, s, pid, tc.body, testBuyer); w.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400", w.Code)
			}
		})
	}

	t.Run("malformed json", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/p/"+pid+"/job", strings.NewReader("{not json"))
		r.Header.Set(buyerHeader, testBuyer)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", w.Code)
		}
	})
}

func TestDispatchUnknownProvider(t *testing.T) {
	s := newTestServer(t)
	w := dispatch(t, s, "prov-nope", map[string]any{"prompt": "x", "max_units": 10}, testBuyer)
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// TestDispatchToOfflineProvider must not create a job: a registered but disconnected provider
// cannot do work, and recording a job would skew the buyer's velocity for nothing.
func TestDispatchToOfflineProvider(t *testing.T) {
	s := newTestServer(t)
	pid := register(t, s, "0.0.5005", 3)

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 10}, testBuyer)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", w.Code)
	}
	if _, calls, _, _ := s.store.BuyerStats(testBuyer, time.Hour); calls != 0 {
		t.Errorf("a job was recorded against an offline provider: %d calls", calls)
	}
}

// TestPolicyDeniesBeforeAnyWork is why the ceiling is checked at dispatch rather than only at
// collect: the thing being protected here is the provider's compute, which the buyer risks nothing
// to consume.
func TestPolicyDeniesBeforeAnyWork(t *testing.T) {
	s := newTestServer(t)
	ran := make(chan struct{}, 1)
	pid, done := connectProvider(t, s, 3, func(f hub.Frame) *hub.Frame {
		if f.Type == hub.FrameJob {
			ran <- struct{}{}
			return &hub.Frame{Type: hub.FrameResult, JobID: f.JobID, Units: 1}
		}
		return nil
	})
	defer done()

	// max_units * rate lands well over the default per-call cap.
	over := s.limits.PerCallCapTinybar/3 + 100
	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": over}, testBuyer)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}

	reason, _ := decodeBody(t, w)["error"].(string)
	if !strings.HasPrefix(reason, policy.PrefixDenied) {
		t.Errorf("error = %q, want the %s class", reason, policy.PrefixDenied)
	}
	var ev map[string]any
	if i := strings.Index(reason, " "); i > 0 {
		json.Unmarshal([]byte(reason[i+1:]), &ev)
	}
	if ev["rule"] != policy.RulePerCallCap {
		t.Errorf("rule = %v, want %s", ev["rule"], policy.RulePerCallCap)
	}

	select {
	case <-ran:
		t.Error("the provider was asked to do work for a job that could never be paid for")
	case <-time.After(200 * time.Millisecond):
	}
	if _, calls, _, _ := s.store.BuyerStats(testBuyer, time.Hour); calls != 0 {
		t.Errorf("a denied dispatch created a job: %d calls", calls)
	}
}

// TestDispatchSurvivesBuyerHangup covers the detached context. A buyer that disconnects mid-job
// must not cancel work the provider is doing and expects to be paid for; the result is held for
// collection either way.
func TestDispatchSurvivesBuyerHangup(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	finished := make(chan string, 1)

	pid, done := connectProvider(t, s, 3, func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		<-release // hold the job open until the buyer has gone away
		finished <- f.JobID
		return &hub.Frame{Type: hub.FrameResult, JobID: f.JobID, Result: "done anyway", Units: 5}
	})
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/p/"+pid+"/job", strings.NewReader(`{"prompt":"x","max_units":100}`)).WithContext(ctx)
	r.Header.Set(buyerHeader, testBuyer)

	go s.routes().ServeHTTP(httptest.NewRecorder(), r)
	time.Sleep(100 * time.Millisecond)
	cancel() // the buyer hangs up
	close(release)

	var jobID string
	select {
	case jobID = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the job was cancelled when the buyer disconnected")
	}

	// The work must land as a completed, billable result waiting to be collected — not merely
	// have run to completion inside the provider.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, err := s.store.Job(jobID, testBuyer); err == nil && j.State == store.JobCompleted {
			if !j.Billable() {
				t.Error("the result is not billable after the buyer hung up")
			}
			if j.Price != 15 {
				t.Errorf("price = %d tinybar, want 15 (5 units at 3)", j.Price)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, err := s.store.Job(jobID, testBuyer)
	t.Fatalf("job never reached completed: %+v (err %v)", j, err)
}
