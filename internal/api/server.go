// Package api is the HTTP layer: router, middleware and handlers for the public
// API (Part II) and the internal/operational endpoints (§18).
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"railway/internal/admission"
	"railway/internal/auth"
	"railway/internal/booking"
	"railway/internal/catalog"
	"railway/internal/live"
	"railway/internal/otp"
	"railway/internal/payment"
	"railway/internal/platform"
	"railway/internal/platform/config"
	"railway/internal/search"
	"railway/internal/seatmirror"
	"railway/internal/ticketsig"
)

// InvariantChecker runs the §24 checks for one journey.
type InvariantChecker func(ctx context.Context, journeyID int64) (map[string]any, error)

type Server struct {
	cfg    config.Config
	log    *slog.Logger
	clock  platform.Clock
	db     *pgxpool.Pool
	readDB *pgxpool.Pool
	rdb    redis.UniversalClient
	cat    *catalog.Catalog

	authSvc   *auth.Service
	signer    *auth.Signer
	verifier  *auth.Verifier
	admit     *admission.Verifier
	localAdm  *admission.LocalIssuer
	producer  *booking.Producer
	waiter    *booking.Waiter
	store     *booking.Store
	pnrs      *booking.PNRGen
	otp       *otp.Service
	mirror    *seatmirror.Mirror
	hub       *live.Hub
	payments  *payment.Service
	search    *search.Service
	tickets   *ticketsig.Signer
	kafka     *kgo.Client
	invariant InvariantChecker

	sem chan struct{}
}

type Deps struct {
	Cfg        config.Config
	Log        *slog.Logger
	DB, ReadDB *pgxpool.Pool
	RDB        redis.UniversalClient
	Cat        *catalog.Catalog
	Auth       *auth.Service
	Signer     *auth.Signer
	Verifier   *auth.Verifier
	Admission  *admission.Verifier
	LocalAdm   *admission.LocalIssuer
	Producer   *booking.Producer
	Waiter     *booking.Waiter
	Store      *booking.Store
	PNRs       *booking.PNRGen
	OTP        *otp.Service
	Mirror     *seatmirror.Mirror
	Hub        *live.Hub
	Payments   *payment.Service
	Search     *search.Service
	Tickets    *ticketsig.Signer
	Kafka      *kgo.Client
	Invariants InvariantChecker
}

func NewServer(d Deps) *Server {
	return &Server{
		cfg: d.Cfg, log: d.Log, clock: platform.SystemClock{}, db: d.DB, readDB: d.ReadDB, rdb: d.RDB, cat: d.Cat,
		authSvc: d.Auth, signer: d.Signer, verifier: d.Verifier, admit: d.Admission, localAdm: d.LocalAdm,
		producer: d.Producer, waiter: d.Waiter, store: d.Store, pnrs: d.PNRs, otp: d.OTP, mirror: d.Mirror, hub: d.Hub,
		payments: d.Payments, search: d.Search, tickets: d.Tickets, kafka: d.Kafka, invariant: d.Invariants,
		sem: make(chan struct{}, max(d.Cfg.MaxInflight, 1)),
	}
}

// Public returns the public router for the given roles ("all" mounts everything).
// Endpoint numbers follow the index in §9.8.
func (s *Server) Public(roles map[string]bool) http.Handler {
	mux := http.NewServeMux()
	has := func(r string) bool { return roles[r] || roles["all"] }
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.instrument(pattern, fn)) }

	if has("auth") {
		h("POST /v1/auth/register", s.register) // 1
		h("POST /v1/auth/login", s.login)       // 2
		h("POST /v1/auth/refresh", s.refresh)   // 3
		h("POST /v1/auth/logout", s.user(s.logout))
		h("GET /v1/auth/captcha", s.captchaConfig)
		h("GET /.well-known/jwks.json", s.jwks)
	}
	if has("search") {
		h("GET /v1/stations", s.stations)                     // 7
		h("GET /v1/search", s.searchTrains)                   // 8
		h("GET /v1/journeys/{id}", s.journey)                 // 9
		h("GET /v1/layouts/{layoutId}", s.layout)             // 10
		h("GET /v1/journeys/{id}/seatmap", s.user(s.seatMap)) // 11: any logged-in user, no admission
	}
	if has("live") {
		// SSE connections are long-lived: they bypass the request semaphore
		// and are capped by LIVE_MAX_CONNS instead. Any logged-in user may
		// watch; admission is needed only to lock seats.
		mux.Handle("GET /v1/journeys/{id}/seatmap/live", s.user(s.liveStream)) // 12
	}
	if has("api") {
		h("GET /v1/me", s.user(s.me)) // 6
		h("GET /v1/me/passengers", s.user(s.listPassengers))
		h("POST /v1/me/passengers", s.user(s.addPassenger))
		h("DELETE /v1/me/passengers/{id}", s.user(s.deletePassenger))
		h("GET /v1/me/bookings", s.user(s.myBookings))                                   // 24
		h("PUT /v1/journeys/{id}/selection/{seatId}", s.admittedUser(s.selectSeat))      // 14
		h("DELETE /v1/journeys/{id}/selection/{seatId}", s.admittedUser(s.unselectSeat)) // 15
		h("GET /v1/journeys/{id}/selection", s.user(s.getSelection))                     // 16
		h("POST /v1/holds", s.admittedUser(s.createHold))                                // 17
		h("POST /v1/holds/{pnr}/otp/verify", s.user(s.verifyOTP))                        // 18
		h("POST /v1/holds/{pnr}/otp/resend", s.user(s.resendOTP))                        // 19
		h("GET /v1/requests/{requestId}", s.user(s.requestStatus))                       // 20
		h("GET /v1/requests/{requestId}/stream", s.user(s.requestStream))
		h("POST /v1/payments", s.user(s.createPayment))              // 21
		h("GET /v1/payments/{paymentId}", s.user(s.getPayment))      // 22
		h("POST /v1/webhooks/payment/{gateway}", s.webhook)          // 23
		h("GET /v1/bookings/{pnr}", s.user(s.getBooking))            // 24
		h("POST /v1/bookings/{pnr}/cancel", s.user(s.cancelBooking)) // 25
		h("GET /v1/bookings/{pnr}/ticket", s.user(s.ticket))         // 26
		h("GET /v1/tickets/keys", s.ticketKeys)
		h("POST /v1/tickets/verify", s.user(s.verifyTicket))
		if s.cfg.AdmissionMode == "local" {
			h("POST /v1/admission", s.user(s.localAdmission)) // 30
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fail(w, r, "NOT_FOUND", "no such endpoint", nil) })
	return s.base(mux)
}

// Internal returns the internal-network router (§18).
func (s *Server) Internal(ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /internal/v1/journeys/{id}/{action}", s.internal(s.journeyCommand))
	mux.HandleFunc("GET /internal/v1/journeys/{id}/invariants", s.internal(s.invariants))
	return s.base(mux)
}
