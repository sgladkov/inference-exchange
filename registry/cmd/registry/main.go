// Command registry is the broker: the only publicly reachable component in the exchange.
//
// It is the x402 resource server, the job dispatcher, and the spend control. Providers dial in
// over a websocket and never listen on a port; buyers dispatch jobs for free, and pay at collect
// once the work is done. Because no payment is in flight while a job runs, job duration is not
// bounded by Hedera's 180-second transaction validity window.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/cryptoscruffy/inference-exchange/registry/internal/hub"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/policy"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/store"
	"github.com/cryptoscruffy/inference-exchange/registry/internal/x402"
)

const network = "hedera:testnet"

// buyerHeader carries the buyer's Hedera account id.
//
// It is asserted, not authenticated, at dispatch. That is acceptable because it is checked where
// it matters: at collect the facilitator reports the account that actually signed the payment, and
// a mismatch against the job's buyer is denied. So an attacker can attribute a free job to someone
// else, but cannot spend their money or read their results.
const buyerHeader = "X-Buyer-Account"

type server struct {
	store  *store.Store
	hub    *hub.Hub
	fac    *x402.Client
	limits policy.Limits
	log    *slog.Logger

	feePayer   string // the facilitator's account, copied into every challenge
	baseURL    string
	jobTimeout time.Duration
}

func main() {
	var (
		addr       = flag.String("addr", ":8080", "listen address")
		facURL     = flag.String("facilitator", envOr("BLOCKY402_URL", "https://api.testnet.blocky402.com"), "x402 facilitator base URL, no /v1 prefix")
		baseURL    = flag.String("base-url", "", "public base URL for resource descriptors (default http://localhost<addr>)")
		jobTimeout = flag.Duration("job-timeout", 30*time.Minute, "how long a dispatched job may run")
		resultTTL  = flag.Duration("result-ttl", 30*time.Minute, "how long a completed result is held awaiting payment")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *baseURL == "" {
		*baseURL = "http://localhost" + *addr
	}

	s := &server{
		store:      store.New(*resultTTL),
		hub:        hub.New(log),
		fac:        x402.NewClient(*facURL, 30*time.Second),
		limits:     policy.Defaults(),
		log:        log,
		baseURL:    *baseURL,
		jobTimeout: *jobTimeout,
	}
	s.hub.OnStateChange = s.store.SetOnline

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Discover the facilitator's fee payer. The buyer sets it as the transaction id's payer
	// account, which is why settlements appear on the mirror node under the facilitator rather
	// than under us. Without it we cannot issue a usable challenge, so this is a hard start
	// requirement — the cached fallback only helps a process that has already succeeded once.
	sup, cached, err := s.fac.Supported(ctx)
	if err != nil {
		log.Error("cannot reach the facilitator on startup", "url", *facURL, "err", err)
		os.Exit(1)
	}
	fp, ok := sup.FeePayerFor(network)
	if !ok {
		log.Error("facilitator advertises no fee payer for this network", "network", network)
		os.Exit(1)
	}
	s.feePayer = fp
	log.Info("facilitator ready", "url", *facURL, "network", network, "feePayer", fp, "from_cache", cached)

	go s.hub.Heartbeat(ctx, 30*time.Second)
	go s.sweep(ctx, time.Minute)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Info("registry listening", "addr", *addr, "base_url", *baseURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /.well-known/ucp", s.handleUCP)
	mux.HandleFunc("POST /providers", s.handleRegister)
	mux.HandleFunc("GET /providers", s.handleListProviders)
	mux.HandleFunc("GET /providers/{id}", s.handleGetProvider)
	mux.HandleFunc("GET /connect", s.handleConnect)
	mux.HandleFunc("POST /p/{id}/job", s.handleDispatch)
	mux.HandleFunc("GET /p/job/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /p/job/{id}", s.handleCollect)
	return logging(s.log, mux)
}

// --- provider registration and connection ----------------------------------

func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID   string         `json:"account_id"`
		PublicKey   string         `json:"public_key"`
		DisplayName string         `json:"display_name"`
		Capability  string         `json:"capability"`
		RatePerUnit int64          `json:"rate_per_unit"`
		Declared    map[string]any `json:"declared"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed registration: "+err.Error())
		return
	}
	if in.AccountID == "" || in.RatePerUnit <= 0 {
		writeErr(w, http.StatusBadRequest, "account_id and a positive rate_per_unit are required")
		return
	}
	if in.Capability == "" {
		in.Capability = "text-generation"
	}
	p := s.store.Register(&store.Provider{
		AccountID: in.AccountID, PublicKey: in.PublicKey, DisplayName: in.DisplayName,
		Capability: in.Capability, RatePerUnit: in.RatePerUnit, Declared: in.Declared,
	})
	s.log.Info("provider registered", "provider", p.ID, "account", p.AccountID, "rate", p.RatePerUnit)
	writeJSON(w, http.StatusCreated, map[string]any{
		"provider_id": p.ID,
		"connect_url": "/connect?provider_id=" + p.ID,
	})
}

func (s *server) handleConnect(w http.ResponseWriter, r *http.Request) {
	pid := r.URL.Query().Get("provider_id")
	if _, err := s.store.Provider(pid); err != nil {
		writeErr(w, http.StatusNotFound, "unknown provider_id")
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		s.log.Warn("websocket upgrade failed", "provider", pid, "err", err)
		return
	}
	// No read deadline: a provider may sit idle for a long time between jobs, and the heartbeat is
	// what detects a dead socket.
	s.hub.Serve(r.Context(), pid, ws)
}

func (s *server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	maxRate, _ := strconv.ParseInt(r.URL.Query().Get("max_rate"), 10, 64)
	online := r.URL.Query().Get("online") == "true"
	writeJSON(w, http.StatusOK, map[string]any{
		"providers": s.store.Providers(r.URL.Query().Get("capability"), maxRate, online),
	})
}

func (s *server) handleGetProvider(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.Provider(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleStatus reports a job's progress and, once it is done, its price. Free, and it never
// discloses the prompt or the result — those are what payment buys.
//
// Since dispatch returns before the work finishes, this is how a buyer learns a job completed, what
// it will cost, and whether it is payable at all.
func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.Job(r.PathValue("id"), r.Header.Get(buyerHeader))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// billable comes from the store rather than being re-derived by each caller: whether a state is
	// payable is one rule, and it belongs in one place.
	writeJSON(w, http.StatusOK, statusOf(j))
}

// statusOf renders a job for the status endpoint. Job's own JSON tags already hide the prompt and
// result; this adds the derived fields a caller would otherwise have to infer.
func statusOf(j *store.Job) map[string]any {
	out := map[string]any{
		"job_id": j.ID, "provider_id": j.ProviderID, "state": j.State,
		"billable": j.Billable(), "terminal": j.State.Terminal(),
		"max_units": j.MaxUnits, "created_at": j.CreatedAt, "expires_at": j.ExpiresAt,
	}
	if j.State == store.JobCompleted || j.State == store.JobCollected {
		out["reported_units"] = j.Reported
		out["priced_units"] = j.Priced
		out["price_tinybar"] = j.Price
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	if j.TxID != "" {
		out["tx_id"] = j.TxID
	}
	return out
}

// --- policy plumbing -------------------------------------------------------

// evaluate assembles the buyer's recent history and asks the policy package for a decision.
//
// Called at dispatch against the buyer's declared ceiling, and again at collect against the actual
// price. Two separate calls, two separate decisions: the amount differs between them and the
// budget can have moved.
func (s *server) evaluate(buyer string, p *store.Provider, amount int64) policy.Decision {
	spend, _, abandoned, completed := s.store.BuyerStats(buyer, 24*time.Hour)
	_, inWindow, _, _ := s.store.BuyerStats(buyer, s.limits.VelocityWindow)
	spendProv, callsProv := s.store.SpendWith(buyer, p.ID)
	return policy.Evaluate(s.limits, policy.Request{
		Buyer: buyer, ProviderID: p.ID, PayTo: p.AccountID, Amount: amount,
		SpendToday: spend, CallsInWindow: inWindow,
		SpendWithProv: spendProv, CallsWithProv: callsProv,
		Abandoned: abandoned, CompletedTotal: completed,
	})
}

func (s *server) sweep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.store.Sweep(); n > 0 {
				s.log.Info("expired uncollected jobs", "count", n)
			}
		}
	}
}

// --- misc ------------------------------------------------------------------

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "network": network, "fee_payer": s.feePayer})
}

func (s *server) handleUCP(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ucp_version": "2026-01",
		"organization": map[string]string{
			"name": "Inference Exchange", "url": s.baseURL,
			"description": "Broker for agent-to-agent inference, settled per call over x402 on Hedera."},
		"primary_capability": "text-generation",
		"capabilities": []map[string]any{{
			"name": "text-generation", "endpoint": "/p/{provider_id}/job",
			"payment": map[string]any{
				"protocol": "x402", "version": x402.Version, "network": network},
		}},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/health" {
			log.Debug("request", "method", r.Method, "path", r.URL.Path, "ms", time.Since(start).Milliseconds())
		}
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
