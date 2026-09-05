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

func status(t *testing.T, s *server, jobID string) map[string]any {
	t.Helper()
	w := do(t, s, "GET", "/p/job/"+jobID+"/status", nil, map[string]string{buyerHeader: testBuyer})
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}
	return decodeBody(t, w)
}

// awaitTerminal polls status until the job stops moving, the way a real buyer does now that
// dispatch returns before the work is finished.
func awaitTerminal(t *testing.T, s *server, jobID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := status(t, s, jobID)
		if st["terminal"] == true || st["state"] == string(store.JobCompleted) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s never reached a terminal state: %v", jobID, status(t, s, jobID))
	return nil
}

// TestDispatchReturnsBeforeTheWorkIsDone is the point of the async shape: the caller gets an id it
// can poll and come back to, rather than waiting on an open connection for the whole job.
func TestDispatchReturnsBeforeTheWorkIsDone(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	pid, done := connectProvider(t, s, 3, func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		<-release
		return &hub.Frame{Type: hub.FrameResult, JobID: f.JobID, Result: "the work product", Units: 10}
	})
	defer done()
	defer close(release)

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 100}, testBuyer)
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202: %s", w.Code, w.Body.String())
	}
	b := decodeBody(t, w)

	jobID, _ := b["job_id"].(string)
	if jobID == "" {
		t.Fatal("no job id returned, so the caller cannot poll or recover")
	}
	if b["state"] != string(store.JobRunning) {
		t.Errorf("state = %v, want running", b["state"])
	}
	// The price cannot be known yet: it depends on units the provider has not reported.
	if _, present := b["price_tinybar"]; present {
		t.Error("dispatch quoted a price before the provider reported any usage")
	}
	for _, k := range []string{"status", "collect"} {
		if got, _ := b[k].(string); !strings.Contains(got, jobID) {
			t.Errorf("%s = %q, want it to carry the job id", k, got)
		}
	}

	// And it is genuinely still running while the provider is held.
	if st := status(t, s, jobID); st["state"] != string(store.JobRunning) {
		t.Errorf("status state = %v, want running", st["state"])
	}
}

func TestJobCompletesAndIsPricedOnStatus(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, answerWith("the work product", 10))
	defer done()

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 100}, testBuyer)
	jobID := decodeBody(t, w)["job_id"].(string)
	st := awaitTerminal(t, s, jobID)

	if st["state"] != string(store.JobCompleted) {
		t.Fatalf("state = %v", st["state"])
	}
	if st["billable"] != true {
		t.Errorf("billable = %v, want true", st["billable"])
	}
	if st["priced_units"] != float64(10) || st["price_tinybar"] != float64(30) {
		t.Errorf("pricing = %v units / %v tinybar, want 10 and 30", st["priced_units"], st["price_tinybar"])
	}
	// Status is free, so it must not hand over what payment buys.
	if body := w.Body.String(); strings.Contains(body, "the work product") {
		t.Error("dispatch leaked the result")
	}
	if raw, _ := json.Marshal(st); strings.Contains(string(raw), "the work product") {
		t.Error("status leaked the result before payment")
	}
}

// TestDispatchClampsAnInflatedReport is the metering defence. The provider counts the units it
// bills for, so an inflated claim must cost the buyer nothing above its ceiling — and the raw claim
// must still be visible, since the aggregate is the only evidence of inflation.
func TestDispatchClampsAnInflatedReport(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, answerWith("result", 1_000_000))
	defer done()

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 50}, testBuyer)
	st := awaitTerminal(t, s, decodeBody(t, w)["job_id"].(string))

	if st["reported_units"] != float64(1_000_000) {
		t.Errorf("reported = %v, want the provider's raw claim retained", st["reported_units"])
	}
	if st["priced_units"] != float64(50) {
		t.Errorf("priced = %v, want it clamped to the buyer's ceiling of 50", st["priced_units"])
	}
	if st["price_tinybar"] != float64(150) {
		t.Errorf("price = %v tinybar, want 150", st["price_tinybar"])
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
	if w.Code != http.StatusAccepted {
		t.Fatalf("code = %d, want 202", w.Code)
	}
	jobID := decodeBody(t, w)["job_id"].(string)
	st := awaitTerminal(t, s, jobID)

	if st["state"] != string(store.JobFailed) {
		t.Fatalf("state = %v, want failed", st["state"])
	}
	if st["billable"] != false {
		t.Errorf("billable = %v, want false", st["billable"])
	}
	if reason, _ := st["error"].(string); !strings.Contains(reason, "the backend exploded") {
		t.Errorf("error = %q, want the provider's own reason", reason)
	}

	j, err := s.store.Job(jobID, testBuyer)
	if err != nil {
		t.Fatal(err)
	}
	if j.Billable() || j.Price != 0 {
		t.Errorf("a failed job is billable at %d tinybar", j.Price)
	}
}

// TestProviderDisconnectMidJobFailsTheJob covers the case a poller most needs bounded: the provider
// dies while working. The job must reach a terminal state rather than sit running forever.
func TestProviderDisconnectMidJobFailsTheJob(t *testing.T) {
	s := newTestServer(t)
	arrived := make(chan struct{})
	pid, done := connectProvider(t, s, 3, func(f hub.Frame) *hub.Frame {
		if f.Type == hub.FrameJob {
			close(arrived)
		}
		return nil // take the job, then never answer
	})

	w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 100}, testBuyer)
	jobID := decodeBody(t, w)["job_id"].(string)

	<-arrived
	done() // the provider vanishes

	st := awaitTerminal(t, s, jobID)
	if st["state"] != string(store.JobFailed) {
		t.Errorf("state = %v, want failed after a disconnect", st["state"])
	}
	if st["billable"] != false {
		t.Error("a job whose provider vanished is billable")
	}
}

func TestDispatchRequiresBuyerHeader(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, answerWith("r", 1))
	defer done()

	if w := dispatch(t, s, pid, map[string]any{"prompt": "x", "max_units": 10}, ""); w.Code != http.StatusBadRequest {
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

// TestJobOutlivesTheRequest is what makes the async shape safe. The work must not be tied to the
// connection that started it — a buyer that hangs up has still commissioned work the provider
// expects to be paid for.
func TestJobOutlivesTheRequest(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	pid, done := connectProvider(t, s, 3, func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		<-release // held until after the request is long gone
		return &hub.Frame{Type: hub.FrameResult, JobID: f.JobID, Result: "done anyway", Units: 5}
	})
	defer done()

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/p/"+pid+"/job",
		strings.NewReader(`{"prompt":"x","max_units":100}`)).WithContext(ctx)
	r.Header.Set(buyerHeader, testBuyer)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)

	jobID := decodeBody(t, w)["job_id"].(string)
	cancel() // the buyer hangs up before the job finishes
	close(release)

	st := awaitTerminal(t, s, jobID)
	if st["state"] != string(store.JobCompleted) {
		t.Fatalf("state = %v, want completed despite the hangup", st["state"])
	}
	if st["billable"] != true || st["price_tinybar"] != float64(15) {
		t.Errorf("the result is not payable: %v", st)
	}
}

// TestConcurrentDispatchesAreIndependent is the shape async makes possible: several jobs in flight
// for one buyer, each landing on its own result.
func TestConcurrentDispatchesAreIndependent(t *testing.T) {
	s := newTestServer(t)
	pid, done := connectProvider(t, s, 3, func(f hub.Frame) *hub.Frame {
		if f.Type != hub.FrameJob {
			return nil
		}
		return &hub.Frame{Type: hub.FrameResult, JobID: f.JobID, Result: f.Prompt, Units: 1}
	})
	defer done()

	s.limits.VelocityCalls = 0 // the rule under test here is correlation, not rate limiting
	ids := map[string]string{}
	for i := range 5 {
		prompt := strings.Repeat("ab", i+1)
		w := dispatch(t, s, pid, map[string]any{"prompt": prompt, "max_units": 100}, testBuyer)
		if w.Code != http.StatusAccepted {
			t.Fatalf("dispatch %d: %d %s", i, w.Code, w.Body.String())
		}
		ids[decodeBody(t, w)["job_id"].(string)] = prompt
	}

	for jobID, prompt := range ids {
		awaitTerminal(t, s, jobID)
		j, err := s.store.Job(jobID, testBuyer)
		if err != nil {
			t.Fatal(err)
		}
		if j.Result != prompt {
			t.Errorf("job %s got another job's result: %q, want %q", jobID, j.Result, prompt)
		}
	}
}
