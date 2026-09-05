package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hcs"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/hub"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/store"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/x402"
)

const testBuyer = "0.0.1001"

// newTestServer builds a server without running main(): no flags, no signal handling, and no
// facilitator call, since the fee payer is the only thing startup discovery contributes.
func newTestServer(t *testing.T) *server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := &server{
		store:      store.New(time.Minute),
		hub:        hub.New(log),
		fac:        x402.NewClient("http://127.0.0.1:1", time.Second), // never reached in this file
		limits:     policy.Defaults(),
		log:        log,
		feePayer:   "0.0.7162784",
		baseURL:    "http://registry.test",
		jobTimeout: time.Minute,
		audit:      hcs.Discard{},
	}
	s.hub.OnStateChange = s.store.SetOnline
	return s
}

func do(t *testing.T, s *server, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, path, strings.NewReader(string(b)))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not a JSON object (%d): %s", w.Code, w.Body.String())
	}
	return out
}

// register adds a provider through the public endpoint and returns its id.
func register(t *testing.T, s *server, account string, rate int64) string {
	t.Helper()
	w := do(t, s, "POST", "/providers", map[string]any{
		"account_id": account, "display_name": "Test", "rate_per_unit": rate}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	return decodeBody(t, w)["provider_id"].(string)
}

func TestHealthReportsTheFeePayer(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/health", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	b := decodeBody(t, w)
	if b["status"] != "ok" || b["network"] != network {
		t.Errorf("health = %v", b)
	}
	// The fee payer belongs in health: it is how an operator confirms the registry is issuing
	// challenges payable through the facilitator rather than through us.
	if b["fee_payer"] != "0.0.7162784" {
		t.Errorf("fee_payer = %v", b["fee_payer"])
	}
}

func TestUCPManifest(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/.well-known/ucp", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	b := decodeBody(t, w)
	if b["ucp_version"] == nil || b["primary_capability"] != "text-generation" {
		t.Errorf("manifest = %v", b)
	}
	caps, ok := b["capabilities"].([]any)
	if !ok || len(caps) == 0 {
		t.Fatalf("no capabilities: %v", b)
	}
	pay := caps[0].(map[string]any)["payment"].(map[string]any)
	if pay["protocol"] != "x402" || pay["network"] != network {
		t.Errorf("payment block = %v", pay)
	}
	if pay["version"] != float64(x402.Version) {
		t.Errorf("advertised x402 version = %v, want %d", pay["version"], x402.Version)
	}
}

func TestRegisterProvider(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "POST", "/providers", map[string]any{
		"account_id": "0.0.5005", "display_name": "Echo", "rate_per_unit": 3,
		"declared": map[string]any{"backend": "echo"}}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d: %s", w.Code, w.Body.String())
	}
	b := decodeBody(t, w)
	pid, _ := b["provider_id"].(string)
	if pid == "" {
		t.Fatal("no provider_id returned")
	}
	// The daemon needs to be told where to dial; making it guess the query parameter would be a
	// second place for the contract to drift.
	if got, _ := b["connect_url"].(string); !strings.Contains(got, pid) {
		t.Errorf("connect_url = %q, want it to carry the provider id", got)
	}
}

func TestRegisterValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
	}{
		{"no account", map[string]any{"rate_per_unit": 3}},
		{"zero rate", map[string]any{"account_id": "0.0.1", "rate_per_unit": 0}},
		{"negative rate", map[string]any{"account_id": "0.0.1", "rate_per_unit": -5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			if w := do(t, s, "POST", "/providers", tc.body, nil); w.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400", w.Code)
			}
		})
	}

	t.Run("malformed json", func(t *testing.T) {
		s := newTestServer(t)
		r := httptest.NewRequest("POST", "/providers", strings.NewReader("{not json"))
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", w.Code)
		}
	})
}

func TestRegisterDefaultsCapability(t *testing.T) {
	s := newTestServer(t)
	pid := register(t, s, "0.0.5005", 3)
	p, err := s.store.Provider(pid)
	if err != nil {
		t.Fatal(err)
	}
	if p.Capability != "text-generation" {
		t.Errorf("capability = %q, want the default", p.Capability)
	}
}

// TestListingNeverExposesAPublicKeyField is a small check with a real point: the listing is the
// public face of a provider, and the registry holds no private key at all. Nothing key-shaped
// should leak into it by accident when the struct grows.
func TestListingNeverExposesAPublicKeyField(t *testing.T) {
	s := newTestServer(t)
	do(t, s, "POST", "/providers", map[string]any{
		"account_id": "0.0.5005", "rate_per_unit": 3, "public_key": "302a300506032b6570"}, nil)

	w := do(t, s, "GET", "/providers", nil, nil)
	if strings.Contains(w.Body.String(), "302a300506032b6570") {
		t.Errorf("a key material field reached the public listing: %s", w.Body.String())
	}
}

func TestListProvidersFilters(t *testing.T) {
	s := newTestServer(t)
	cheap := register(t, s, "0.0.1", 1)
	register(t, s, "0.0.2", 100)
	s.store.SetOnline(cheap, true)

	count := func(query string) int {
		t.Helper()
		w := do(t, s, "GET", "/providers"+query, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d", w.Code)
		}
		list, _ := decodeBody(t, w)["providers"].([]any)
		return len(list)
	}
	if got := count(""); got != 2 {
		t.Errorf("unfiltered = %d, want 2", got)
	}
	if got := count("?max_rate=50"); got != 1 {
		t.Errorf("max_rate = %d, want 1", got)
	}
	if got := count("?online=true"); got != 1 {
		t.Errorf("online = %d, want 1", got)
	}
	if got := count("?capability=image"); got != 0 {
		t.Errorf("capability filter = %d, want 0", got)
	}
	// A non-numeric max_rate must not silently exclude everything.
	if got := count("?max_rate=abc"); got != 2 {
		t.Errorf("unparseable max_rate = %d, want it ignored (2)", got)
	}
}

func TestGetProvider(t *testing.T) {
	s := newTestServer(t)
	pid := register(t, s, "0.0.5005", 3)

	w := do(t, s, "GET", "/providers/"+pid, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	if decodeBody(t, w)["pay_to"] != "0.0.5005" {
		t.Errorf("listing = %s", w.Body.String())
	}
	if w := do(t, s, "GET", "/providers/prov-nope", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown provider code = %d, want 404", w.Code)
	}
}

func TestConnectRejectsUnknownProvider(t *testing.T) {
	s := newTestServer(t)
	if w := do(t, s, "GET", "/connect?provider_id=prov-nope", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
	if w := do(t, s, "GET", "/connect", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("missing provider_id code = %d, want 404", w.Code)
	}
}

// TestConnectUpgradesAndMarksOnline drives the real websocket path, so the wiring between the
// route, the hub and the store's online flag is covered rather than assumed.
func TestConnectUpgradesAndMarksOnline(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	pid := register(t, s, "0.0.5005", 3)

	ws, _, err := websocket.Dial(t.Context(), "ws"+srv.URL[4:]+"/connect?provider_id="+pid, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	for range 100 {
		if s.hub.Online(pid) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !s.hub.Online(pid) {
		t.Fatal("provider never registered with the hub")
	}
	p, _ := s.store.Provider(pid)
	if !p.Online {
		t.Error("the store was not told the provider came online")
	}
}

func TestStatusUnknownJob(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s, "GET", "/p/job/job-nope/status", nil, map[string]string{buyerHeader: testBuyer})
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

// TestStatusRefusesAnotherBuyer is the ownership check surfacing at the HTTP layer. A wrong buyer
// must not be able to learn a job exists.
func TestStatusRefusesAnotherBuyer(t *testing.T) {
	s := newTestServer(t)
	pid := register(t, s, "0.0.5005", 3)
	j, err := s.store.CreateJob(pid, testBuyer, "prompt", 100)
	if err != nil {
		t.Fatal(err)
	}

	if w := do(t, s, "GET", "/p/job/"+j.ID+"/status", nil, map[string]string{buyerHeader: testBuyer}); w.Code != http.StatusOK {
		t.Errorf("the owner was refused: %d", w.Code)
	}
	w := do(t, s, "GET", "/p/job/"+j.ID+"/status", nil, map[string]string{buyerHeader: "0.0.9999"})
	if w.Code != http.StatusNotFound {
		t.Errorf("another buyer got %d, want 404", w.Code)
	}
}

// TestStatusNeverLeaksThePrompt guards the JSON tags on the job struct. The prompt and the result
// are the paid-for content; status is a free endpoint.
func TestStatusNeverLeaksThePrompt(t *testing.T) {
	s := newTestServer(t)
	pid := register(t, s, "0.0.5005", 3)
	j, _ := s.store.CreateJob(pid, testBuyer, "the secret prompt", 100)
	if _, err := s.store.Complete(j.ID, 10, "the paid-for result", 3); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, "GET", "/p/job/"+j.ID+"/status", nil, map[string]string{buyerHeader: testBuyer})
	body := w.Body.String()
	if strings.Contains(body, "the secret prompt") {
		t.Errorf("status leaked the prompt: %s", body)
	}
	if strings.Contains(body, "the paid-for result") {
		t.Errorf("status leaked the result before payment: %s", body)
	}
	// It should still say what the job will cost, so a buyer can decide before paying.
	if b := decodeBody(t, w); b["price_tinybar"] != float64(30) {
		t.Errorf("price not reported: %v", b)
	}
}

func TestUnknownRoutes(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/", "/nope", "/providers/x/y", "/p/job"} {
		if w := do(t, s, "GET", path, nil, nil); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, w.Code)
		}
	}
	// The method matters: registration is POST only.
	if w := do(t, s, "PUT", "/providers", map[string]any{}, nil); w.Code == http.StatusCreated {
		t.Error("PUT /providers registered a provider")
	}
}

func TestWriteHelpers(t *testing.T) {
	w := httptest.NewRecorder()
	writeErr(w, http.StatusTeapot, "nope")
	if w.Code != http.StatusTeapot {
		t.Errorf("code = %d", w.Code)
	}
	if ct := w.Header().Get("content-type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] != "nope" {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestEnvOr(t *testing.T) {
	if got := envOr("IX_TEST_UNSET_VAR", "fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
	t.Setenv("IX_TEST_SET_VAR", "value")
	if got := envOr("IX_TEST_SET_VAR", "fallback"); got != "value" {
		t.Errorf("got %q", got)
	}
}

// TestEvaluateUsesTheRightWindows checks the policy plumbing reads the buyer's history correctly:
// daily spend over 24h, velocity over its own window.
func TestEvaluateUsesTheRightWindows(t *testing.T) {
	s := newTestServer(t)
	pid := register(t, s, "0.0.5005", 3)
	p, _ := s.store.Provider(pid)

	if d := s.evaluate(hcs.PhaseDispatch, "", testBuyer, p, 100); d.Deny {
		t.Fatalf("a first ordinary payment was denied: %s", d.Reason)
	}
	if d := s.evaluate(hcs.PhaseDispatch, "", testBuyer, p, s.limits.PerCallCapTinybar+1); !d.Deny || d.Rule != policy.RulePerCallCap {
		t.Errorf("over-cap decision = %+v", d)
	}

	// Velocity counts dispatched jobs, not just settled ones, because dispatch is the free thing
	// worth rate limiting.
	s.limits.VelocityCalls = 2
	for range 2 {
		if _, err := s.store.CreateJob(pid, testBuyer, "x", 10); err != nil {
			t.Fatal(err)
		}
	}
	if d := s.evaluate(hcs.PhaseDispatch, "", testBuyer, p, 1); !d.Deny || d.Rule != policy.RuleVelocity {
		t.Errorf("velocity decision = %+v", d)
	}
}
