package hub

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const providerID = "prov-test"

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeProvider stands in for the daemon: it dials the hub and answers job frames with whatever the
// supplied handler returns. Returning a nil frame means "go silent", which is how a hung provider
// is simulated.
type fakeProvider struct {
	srv  *httptest.Server
	ws   *websocket.Conn
	done chan struct{}
}

func dial(t *testing.T, h *Hub, id string, reply func(Frame) *Frame) *fakeProvider {
	t.Helper()
	fp := &fakeProvider{done: make(chan struct{})}

	fp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		h.Serve(r.Context(), id, ws)
	}))

	ws, _, err := websocket.Dial(context.Background(), "ws"+fp.srv.URL[4:], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	fp.ws = ws

	go func() {
		defer close(fp.done)
		for {
			var f Frame
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

	// The hub registers the connection inside Serve, so wait for it rather than racing the dial.
	for range 100 {
		if h.Online(id) {
			return fp
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("provider %s never came online", id)
	return nil
}

func (fp *fakeProvider) hangup() {
	fp.ws.Close(websocket.StatusNormalClosure, "bye")
	<-fp.done
}

func (fp *fakeProvider) stop() {
	fp.ws.CloseNow()
	fp.srv.Close()
}

// echoBack answers every job with a fixed result and unit count.
func echoBack(units int64) func(Frame) *Frame {
	return func(f Frame) *Frame {
		if f.Type != FrameJob {
			return nil
		}
		return &Frame{Type: FrameResult, JobID: f.JobID, Result: "did: " + f.Prompt, Units: units}
	}
}

func TestDispatchRoundTrip(t *testing.T) {
	h := New(quietLogger())
	fp := dial(t, h, providerID, echoBack(42))
	defer fp.stop()

	out, err := h.Dispatch(context.Background(), providerID, "job-1", "summarise this", 100)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if out.Result != "did: summarise this" {
		t.Errorf("result = %q", out.Result)
	}
	if out.Units != 42 {
		t.Errorf("units = %d, want 42", out.Units)
	}
}

// TestMaxUnitsReachesTheProvider checks the ceiling travels, so a provider can truncate work it
// cannot do within budget. It is advisory there — the registry enforces it at pricing time.
func TestMaxUnitsReachesTheProvider(t *testing.T) {
	h := New(quietLogger())
	got := make(chan int64, 1)
	fp := dial(t, h, providerID, func(f Frame) *Frame {
		if f.Type != FrameJob {
			return nil
		}
		got <- f.MaxUnits
		return &Frame{Type: FrameResult, JobID: f.JobID, Units: 1}
	})
	defer fp.stop()

	if _, err := h.Dispatch(context.Background(), providerID, "job-1", "x", 777); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if v := <-got; v != 777 {
		t.Errorf("provider saw max_units = %d, want 777", v)
	}
}

func TestDispatchToOfflineProvider(t *testing.T) {
	h := New(quietLogger())
	_, err := h.Dispatch(context.Background(), "prov-nobody", "job-1", "x", 10)
	if !errors.Is(err, ErrOffline) {
		t.Errorf("err = %v, want ErrOffline", err)
	}
}

// TestProviderFailureIsAnError covers the branch that keeps a buyer unbilled: the provider says it
// could not do the work, and that must surface as an error rather than an empty result.
func TestProviderFailureIsAnError(t *testing.T) {
	h := New(quietLogger())
	fp := dial(t, h, providerID, func(f Frame) *Frame {
		if f.Type != FrameJob {
			return nil
		}
		return &Frame{Type: FrameFailed, JobID: f.JobID, Error: "the backend exploded"}
	})
	defer fp.stop()

	out, err := h.Dispatch(context.Background(), providerID, "job-1", "x", 10)
	if err == nil {
		t.Fatal("a provider failure returned no error; the buyer would be billed for nothing")
	}
	if out.Result != "" {
		t.Errorf("a failed job carried a result: %q", out.Result)
	}
	if !errors.Is(out.Err, err) && out.Err.Error() != err.Error() {
		t.Errorf("Outcome.Err and the returned error disagree: %v vs %v", out.Err, err)
	}
}

// TestHangupMidJobIsDistinct is why failAll exists. Without it a provider that dies mid-job would
// surface as a timeout minutes later, and the caller could not tell "it crashed, try another" from
// "it is slow, wait".
func TestHangupMidJobIsDistinct(t *testing.T) {
	h := New(quietLogger())
	arrived := make(chan struct{})
	fp := dial(t, h, providerID, func(f Frame) *Frame {
		if f.Type == FrameJob {
			close(arrived)
		}
		return nil // accept the job, then never answer
	})
	defer fp.stop()

	errCh := make(chan error, 1)
	go func() {
		_, err := h.Dispatch(context.Background(), providerID, "job-1", "x", 10)
		errCh <- err
	}()

	<-arrived
	fp.hangup()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrHangup) {
			t.Errorf("err = %v, want ErrHangup", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a disconnect mid-job did not release the waiter")
	}
}

func TestTimeoutWhileProviderIsSilent(t *testing.T) {
	h := New(quietLogger())
	fp := dial(t, h, providerID, func(Frame) *Frame { return nil })
	defer fp.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := h.Dispatch(ctx, providerID, "job-1", "x", 10); !errors.Is(err, ErrTimedOut) {
		t.Errorf("err = %v, want ErrTimedOut", err)
	}
}

func TestDuplicateJobIDRejected(t *testing.T) {
	h := New(quietLogger())
	fp := dial(t, h, providerID, func(Frame) *Frame { return nil })
	defer fp.stop()

	go h.Dispatch(context.Background(), providerID, "job-dup", "x", 10)
	// Let the first dispatch register its pending entry.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.Dispatch(ctx, providerID, "job-dup", "x", 10); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
}

// TestReconnectReplacesOldConnection covers a provider restarting. The new socket must take over
// without waiting for the old one to time out, and the old one's in-flight work must be released.
func TestReconnectReplacesOldConnection(t *testing.T) {
	h := New(quietLogger())
	var states []bool
	var mu sync.Mutex
	h.OnStateChange = func(_ string, online bool) {
		mu.Lock()
		states = append(states, online)
		mu.Unlock()
	}

	first := dial(t, h, providerID, func(Frame) *Frame { return nil })
	defer first.stop()

	errCh := make(chan error, 1)
	go func() {
		_, err := h.Dispatch(context.Background(), providerID, "job-1", "x", 10)
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond)

	second := dial(t, h, providerID, echoBack(7))
	defer second.stop()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrHangup) {
			t.Errorf("the replaced connection's job ended with %v, want ErrHangup", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacing a connection left its job hanging")
	}

	if !h.Online(providerID) {
		t.Fatal("provider went offline after reconnecting")
	}
	out, err := h.Dispatch(context.Background(), providerID, "job-2", "x", 10)
	if err != nil || out.Units != 7 {
		t.Errorf("the replacement connection does not serve: %v %+v", err, out)
	}

	// The old connection's teardown must not report the provider offline: a newer one owns the
	// registration by then.
	mu.Lock()
	defer mu.Unlock()
	if len(states) == 0 || !states[len(states)-1] {
		t.Errorf("state changes = %v, want the last to be online", states)
	}
}

func TestOnStateChangeFires(t *testing.T) {
	h := New(quietLogger())
	seen := make(chan bool, 4)
	h.OnStateChange = func(_ string, online bool) { seen <- online }

	fp := dial(t, h, providerID, func(Frame) *Frame { return nil })
	if online := <-seen; !online {
		t.Error("connect did not report online")
	}
	fp.hangup()
	select {
	case online := <-seen:
		if online {
			t.Error("disconnect reported online")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect never reported")
	}
	fp.stop()
}

func TestUnknownFrameIsIgnored(t *testing.T) {
	h := New(quietLogger())
	fp := dial(t, h, providerID, func(f Frame) *Frame {
		if f.Type != FrameJob {
			return nil
		}
		// A frame the hub does not know must not kill the session or the pending job.
		return &Frame{Type: "some-future-frame", JobID: f.JobID}
	})
	defer fp.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := h.Dispatch(ctx, providerID, "job-1", "x", 10); !errors.Is(err, ErrTimedOut) {
		t.Errorf("err = %v, want the job still pending and then timing out", err)
	}
	if !h.Online(providerID) {
		t.Error("an unknown frame tore down the connection")
	}
}

// TestConcurrentDispatch is a race-detector target: many jobs in flight over one socket, where all
// writes share a single mutex and every reply must reach its own waiter.
func TestConcurrentDispatch(t *testing.T) {
	h := New(quietLogger())
	fp := dial(t, h, providerID, func(f Frame) *Frame {
		if f.Type != FrameJob {
			return nil
		}
		return &Frame{Type: FrameResult, JobID: f.JobID, Result: f.Prompt, Units: 1}
	})
	defer fp.stop()

	var wg sync.WaitGroup
	for i := range 25 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			jobID := "job-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := h.Dispatch(ctx, providerID, jobID, jobID, 10)
			if err != nil {
				t.Errorf("%s: %v", jobID, err)
				return
			}
			// Each reply must land on its own waiter, not somebody else's.
			if out.Result != jobID {
				t.Errorf("%s got another job's result: %q", jobID, out.Result)
			}
		}(i)
	}
	wg.Wait()
}

func TestMarshalFrame(t *testing.T) {
	b, err := MarshalFrame(Frame{Type: FrameJob, JobID: "job-1", Prompt: "hi", MaxUnits: 5})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"type":"job"`, `"job_id":"job-1"`, `"max_units":5`} {
		if !strings.Contains(got, want) {
			t.Errorf("frame %s missing %s", got, want)
		}
	}
	// Empty optional fields are omitted, so the daemon is not handed misleading zero values.
	if strings.Contains(got, `"result"`) || strings.Contains(got, `"error"`) {
		t.Errorf("job frame carried empty response fields: %s", got)
	}
}
