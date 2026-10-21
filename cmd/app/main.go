// Command app is the modular monolith: one binary run with role flags
// (--role=api|live|auth|search|projector|relay|reconciler|scheduler, comma-separated,
// or "all" for local development), plus the migrate, keygen and invariants tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"railway/internal/admission"
	"railway/internal/allocator"
	"railway/internal/api"
	"railway/internal/auth"
	"railway/internal/booking"
	"railway/internal/catalog"
	"railway/internal/live"
	"railway/internal/notify"
	"railway/internal/otp"
	"railway/internal/outbox"
	"railway/internal/payment"
	"railway/internal/platform"
	"railway/internal/platform/config"
	"railway/internal/platform/kafka"
	"railway/internal/platform/pg"
	"railway/internal/platform/redisx"
	"railway/internal/projector"
	"railway/internal/scheduler"
	"railway/internal/search"
	"railway/internal/seatmirror"
	"railway/internal/ticketsig"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			exit(runMigrate(os.Args[2:]))
		case "keygen":
			exit(runKeygen(os.Args[2:]))
		case "invariants":
			exit(runInvariants(os.Args[2:]))
		}
	}
	fs := flag.NewFlagSet("app", flag.ExitOnError)
	role := fs.String("role", "", "api|live|auth|search|projector|relay|reconciler|scheduler|all (comma-separated)")
	_ = fs.Parse(os.Args[1:])
	exit(serve(*role))
}

func exit(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type runtime struct {
	cfg    config.Config
	log    *slog.Logger
	db     *pgxpool.Pool
	readDB *pgxpool.Pool
	rdb    redis.UniversalClient
	cat    *catalog.Catalog
	prod   *kgo.Client
}

func setup(ctx context.Context) (*runtime, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	rt := &runtime{cfg: cfg, log: platform.NewLogger(cfg.LogLevel)}
	if rt.db, err = pg.Connect(ctx, cfg.PGURL); err != nil {
		return nil, err
	}
	rt.readDB = rt.db
	if cfg.PGReadURL != cfg.PGURL {
		if rt.readDB, err = pg.Connect(ctx, cfg.PGReadURL); err != nil {
			return nil, err
		}
	}
	rt.rdb = redisx.New(cfg.RedisAddr)
	rt.cat = catalog.New(rt.readDB, rt.log)
	if err := rt.cat.Load(ctx); err != nil {
		return nil, fmt.Errorf("load catalog: %w", err)
	}
	go rt.cat.Run(ctx, 5*time.Minute)
	return rt, nil
}

func serve(roleFlag string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rt, err := setup(ctx)
	if err != nil {
		return err
	}
	cfg, log := rt.cfg, rt.log
	if roleFlag == "" {
		roleFlag = cfg.Role
	}
	roles := map[string]bool{}
	for _, r := range strings.Split(roleFlag, ",") {
		roles[strings.TrimSpace(r)] = true
	}
	has := func(r string) bool { return roles[r] || roles["all"] }
	log = log.With("role", roleFlag, "pod", cfg.PodName)
	log.Info("starting")

	rt.prod, err = kafka.NewProducer(cfg.KafkaBrokers)
	if err != nil {
		return err
	}
	defer rt.prod.Close()
	producer := booking.NewProducer(rt.prod)
	store := booking.NewStore(rt.rdb)
	clock := platform.SystemClock{}
	loader := allocator.NewLoader(cfg, rt.cat, rt.db)

	var wg sync.WaitGroup
	goRun := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("component stopped", "component", name, "err", err)
				stop()
			}
		}()
	}

	// Identity: every HTTP role verifies access tokens locally.
	verifier := auth.NewVerifier(rt.rdb, log)
	var signer *auth.Signer
	if cfg.JWTSigningKeyFile != "" {
		key, err := auth.LoadECKey(cfg.JWTSigningKeyFile)
		if err != nil {
			return fmt.Errorf("jwt signing key: %w", err)
		}
		signer = auth.NewSigner(cfg.JWTKID, key, cfg.AccessTTL)
		verifier.AddKey(cfg.JWTKID, &key.PublicKey)
	} else if has("auth") {
		return errors.New("auth role needs JWT_SIGNING_KEY_FILE")
	}
	if cfg.JWKSURL != "" {
		go verifier.RunJWKS(ctx, cfg.JWKSURL)
	}
	go verifier.RunDenylist(ctx)

	// Admission.
	var admVerifier *admission.Verifier
	var localAdm *admission.LocalIssuer
	if cfg.AdmissionMode == "local" && cfg.AdmissionKeyFile != "" {
		key, err := auth.LoadECKey(cfg.AdmissionKeyFile)
		if err != nil {
			return fmt.Errorf("admission key: %w", err)
		}
		localAdm = admission.NewLocalIssuer(key, rt.rdb, cfg.AdmissionRate)
		admVerifier = admission.NewVerifier(&key.PublicKey, rt.rdb)
	} else if cfg.AdmissionPubKey != "" {
		pub, err := auth.ParseECPublic(cfg.AdmissionPubKey)
		if err != nil {
			return fmt.Errorf("admission public key: %w", err)
		}
		admVerifier = admission.NewVerifier(pub, rt.rdb)
	} else if has("api") {
		return errors.New("api role needs ADMISSION_PUBKEY (or ADMISSION_MODE=local with ADMISSION_KEY_FILE)")
	}

	// E-ticket QR signing (api role).
	var tickets *ticketsig.Signer
	if cfg.TicketSigningKeyFile != "" {
		key, err := auth.LoadECKey(cfg.TicketSigningKeyFile)
		if err != nil {
			return fmt.Errorf("ticket signing key: %w", err)
		}
		tickets = ticketsig.NewSigner(cfg.TicketKID, key)
	} else if has("api") {
		return errors.New("api role needs TICKET_SIGNING_KEY_FILE")
	}

	var authSvc *auth.Service
	if has("auth") {
		authSvc = &auth.Service{DB: rt.db, ReadDB: rt.readDB, RDB: rt.rdb,
			Hasher: auth.NewHasher(cfg.Argon2MemoryKiB, cfg.Argon2Time, cfg.Argon2Threads),
			Signer: signer, AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL, Now: clock.Now, NIDKey: []byte(cfg.NIDKey)}
	}
	if has("api") && cfg.OTPKey == "" {
		return errors.New("api role needs OTP_KEY")
	}
	otpSvc := &otp.Service{DB: rt.db, RDB: rt.rdb, Key: []byte(cfg.OTPKey), TTL: cfg.OTPTTL,
		MaxAttempts: cfg.OTPMaxAttempts, MaxResends: cfg.OTPMaxResends, Clock: clock, DevCode: cfg.OTPDevCode}
	if has("api") && cfg.OTPDevCode != "" {
		log.Warn("OTP_DEV_CODE is set: that code verifies every booking. Never set it in production.")
	}
	mirror := seatmirror.New(rt.rdb, log)
	hub := live.NewHub(rt.rdb, log, cfg.LiveMaxConns)
	if has("api") {
		go mirror.Run(ctx)
	}
	if has("live") {
		go hub.Run(ctx)
	}

	gw := payment.NewFakeAdapter(cfg.GatewayName(), cfg.GatewayBaseURL, cfg.GatewaySecret)
	paySvc := &payment.Service{DB: rt.db, ReadDB: rt.readDB, RDB: rt.rdb, Store: store, Cat: rt.cat, GW: gw, GWName: cfg.GatewayName(), Log: log, Clock: clock}
	refunds := &payment.Refunds{DB: rt.db, GW: gw, Log: log}

	waiter := booking.NewWaiter()
	if has("api") {
		go waiter.Run(ctx, rt.rdb, cfg.PodName, log)
	}

	srv := api.NewServer(api.Deps{
		Cfg: cfg, Log: log, DB: rt.db, ReadDB: rt.readDB, RDB: rt.rdb, Cat: rt.cat,
		Auth: authSvc, Signer: signer, Verifier: verifier, Admission: admVerifier, LocalAdm: localAdm,
		Producer: producer, Waiter: waiter, Store: store, PNRs: booking.NewPNRGen(rt.rdb), OTP: otpSvc, Mirror: mirror, Hub: hub,
		Payments: paySvc, Search: &search.Service{Cat: rt.cat, Store: store, Clock: clock}, Tickets: tickets, Kafka: rt.prod,
		Invariants: func(ctx context.Context, id int64) (map[string]any, error) {
			return allocator.CheckJourney(ctx, loader, rt.db, rt.rdb, id)
		},
	})

	ready := func(ctx context.Context) error {
		if err := rt.db.Ping(ctx); err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		if err := redisx.Ping(ctx, rt.rdb); err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		if err := kafka.Ping(ctx, rt.prod); err != nil {
			return fmt.Errorf("kafka: %w", err)
		}
		if (has("api") || has("live") || has("search") || has("auth")) && !verifier.HasKeys() {
			return errors.New("jwks not loaded")
		}
		if has("api") && !waiter.Ready() {
			return errors.New("results subscription not ready")
		}
		if has("live") && hub.Full() {
			return errors.New("live connections at LIVE_MAX_CONNS")
		}
		return nil
	}

	var servers []*http.Server
	if has("api") || has("live") || has("auth") || has("search") {
		servers = append(servers, &http.Server{Addr: cfg.HTTPAddr, Handler: srv.Public(roles), ReadHeaderTimeout: 5 * time.Second})
	}
	servers = append(servers, &http.Server{Addr: cfg.InternalAddr, Handler: srv.Internal(ready), ReadHeaderTimeout: 5 * time.Second})
	for _, s := range servers {
		s := s
		go func() {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http server", "addr", s.Addr, "err", err)
				stop()
			}
		}()
	}

	if has("projector") {
		goRun("projector", (&projector.Projector{Brokers: cfg.KafkaBrokers, DB: rt.db, RDB: rt.rdb, Cat: rt.cat, Log: log}).Run)
	}
	if has("relay") {
		goRun("relay", (&outbox.Relay{DB: rt.db, Cl: rt.prod, Log: log}).Run)
		goRun("notifier", (&notify.Notifier{Brokers: cfg.KafkaBrokers, DB: rt.db, SMS: notify.NewHTTPSender(cfg.SMSProviders), Log: log}).Run)
	}
	if has("reconciler") {
		goRun("refunder", (&payment.Refunder{Brokers: cfg.KafkaBrokers, DB: rt.db, Store: store, Cat: rt.cat, Refunds: refunds, Log: log}).Run)
		rec := &payment.Reconciler{DB: rt.db, Svc: paySvc, Refunds: refunds, Log: log}
		goRun("reconciler", func(ctx context.Context) error { return pg.RunAsLeader(ctx, rt.db, 43, log, rec.Run) })
	}
	if has("scheduler") {
		goRun("scheduler", (&scheduler.Scheduler{DB: rt.db, Producer: producer, Log: log}).Run)
	}

	<-ctx.Done()
	log.Info("shutting down: draining in-flight requests")
	hub.Shutdown() // live clients reconnect to another pod first
	sctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	wg.Wait()
	return nil
}
