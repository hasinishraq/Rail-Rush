// Package config parses every setting from environment variables (12-factor)
// into one Config struct (§23).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Role         string
	HTTPAddr     string
	InternalAddr string
	PodName      string

	KafkaBrokers []string
	Partitions   int
	PGURL        string
	PGReadURL    string
	RedisAddr    string

	// allocator
	SelectIdleTTL          time.Duration
	SelectMaxTTL           time.Duration
	MaxSeatsPerUser        int // default for journey_pool.max_seats_per_user (seed)
	HoldTTL                time.Duration
	LatePayGrace           time.Duration
	OutageExtendAfter      time.Duration
	SnapshotEvery          int
	SnapshotInterval       time.Duration
	LiveDeltaInterval      time.Duration
	SeatmapPublishInterval time.Duration
	SmlogMaxLen            int
	IdleEvictAfter         time.Duration

	// api
	DailySeatLimit int
	ResultWait     time.Duration
	LagRejectMS    int
	MaxInflight    int
	ReturnURLs     []string // allow-listed return_url prefixes for payments
	TrustProxy     bool     // take the client IP from X-Forwarded-For

	// live
	LiveMaxConns   int
	LiveConnMaxAge time.Duration

	// otp / sms
	OTPTTL         time.Duration
	OTPMaxAttempts int
	OTPMaxResends  int
	OTPKey         string
	OTPDevCode     string // local only: a code accepted for every PNR
	NIDKey         string
	SMSProviders   []string

	// admission
	AdmissionMode     string // vendor | local
	AdmissionPubKey   string // PEM or path to PEM
	AdmissionKeyFile  string // local mode: ES256 private key used to sign tokens
	AdmissionRate     float64
	AdmissionQueueURL string

	// auth
	JWTSigningKeyFile string
	JWTKID            string
	JWKSURL           string // optional: where non-auth roles fetch JWKS from
	AccessTTL         time.Duration
	RefreshTTL        time.Duration
	Argon2MemoryKiB   uint32
	Argon2Time        uint32
	Argon2Threads     uint8
	TurnstileSiteKey  string // Cloudflare Turnstile; empty secret disables the check
	TurnstileSecret   string

	// tickets
	TicketSigningKeyFile string // ES256 key that signs the e-ticket QR payload (api role)
	TicketKID            string

	// payments
	Gateways         []string // enabled adapters; "fake" locally
	GatewayBaseURL   string
	GatewayPublicURL string
	GatewaySecret    string

	InternalToken string
	LogLevel      string
	OTLPEndpoint  string
}

// Load reads the environment. Missing required values are reported together.
func Load() (Config, error) {
	host, _ := os.Hostname()
	c := Config{
		Role:         env("ROLE", "api"),
		HTTPAddr:     env("HTTP_ADDR", ":8080"),
		InternalAddr: env("INTERNAL_ADDR", ":9091"),
		PodName:      env("POD_NAME", host),

		KafkaBrokers: splitList(env("KAFKA_BROKERS", "redpanda:9092")),
		Partitions:   envInt("PARTITIONS", 256),
		PGURL:        env("PG_URL", ""),
		RedisAddr:    env("REDIS_ADDR", "redis:6379"),

		SelectIdleTTL:          envDur("SELECT_IDLE_TTL", 2*time.Minute),
		SelectMaxTTL:           envDur("SELECT_MAX_TTL", 5*time.Minute),
		MaxSeatsPerUser:        envInt("MAX_SEATS_PER_USER", 4),
		HoldTTL:                envDur("HOLD_TTL", 10*time.Minute),
		LatePayGrace:           envDur("LATE_PAY_GRACE", 30*time.Second),
		OutageExtendAfter:      envDur("OUTAGE_EXTEND_AFTER", 30*time.Second),
		SnapshotEvery:          envInt("SNAPSHOT_EVERY", 5000),
		SnapshotInterval:       envDur("SNAPSHOT_INTERVAL", 60*time.Second),
		LiveDeltaInterval:      envDur("LIVE_DELTA_INTERVAL", 250*time.Millisecond),
		SeatmapPublishInterval: envDur("SEATMAP_PUBLISH_INTERVAL", time.Second),
		SmlogMaxLen:            envInt("SMLOG_MAXLEN", 2000),
		IdleEvictAfter:         envDur("IDLE_EVICT_AFTER", 30*time.Minute),

		DailySeatLimit: envInt("DAILY_SEAT_LIMIT", 4),
		ResultWait:     envDur("RESULT_WAIT", 2*time.Second),
		LagRejectMS:    envInt("LAG_REJECT_MS", 2000),
		MaxInflight:    envInt("MAX_INFLIGHT", 2000),
		ReturnURLs:     splitList(env("RETURN_URL_ALLOWLIST", "http://localhost,https://localhost")),
		TrustProxy:     envBool("TRUST_PROXY", true),

		LiveMaxConns:   envInt("LIVE_MAX_CONNS", 50000),
		LiveConnMaxAge: envDur("LIVE_CONN_MAX_AGE", 15*time.Minute),

		OTPTTL:         envDur("OTP_TTL", 3*time.Minute),
		OTPMaxAttempts: envInt("OTP_MAX_ATTEMPTS", 5),
		OTPMaxResends:  envInt("OTP_MAX_RESENDS", 3),
		OTPKey:         env("OTP_KEY", ""),
		OTPDevCode:     env("OTP_DEV_CODE", ""),
		NIDKey:         env("NID_KEY", ""),
		SMSProviders:   splitList(env("SMS_PROVIDERS", "")),

		AdmissionMode:     env("ADMISSION_MODE", "vendor"),
		AdmissionPubKey:   env("ADMISSION_PUBKEY", ""),
		AdmissionKeyFile:  env("ADMISSION_KEY_FILE", ""),
		AdmissionRate:     envFloat("ADMISSION_RATE", 200),
		AdmissionQueueURL: env("ADMISSION_QUEUE_URL", "/queue/status"),

		JWTSigningKeyFile: env("JWT_SIGNING_KEY_FILE", ""),
		JWTKID:            env("JWT_KID", "dev1"),
		JWKSURL:           env("JWKS_URL", ""),
		AccessTTL:         envDur("ACCESS_TTL", 15*time.Minute),
		RefreshTTL:        envDur("REFRESH_TTL", 720*time.Hour),
		Argon2MemoryKiB:   uint32(envInt("ARGON2_MEMORY_KIB", 65536)),
		Argon2Time:        uint32(envInt("ARGON2_TIME", 1)),
		Argon2Threads:     uint8(envInt("ARGON2_THREADS", 2)),
		TurnstileSiteKey:  env("TURNSTILE_SITE_KEY", ""),
		TurnstileSecret:   env("TURNSTILE_SECRET_KEY", ""),

		TicketSigningKeyFile: env("TICKET_SIGNING_KEY_FILE", ""),
		TicketKID:            env("TICKET_KID", "t1"),

		Gateways:         splitList(env("GATEWAYS", "fake")),
		GatewayBaseURL:   env("GATEWAY_BASE_URL", "http://fakegateway:8090"),
		GatewayPublicURL: env("GATEWAY_PUBLIC_URL", ""),
		GatewaySecret:    env("GATEWAY_SECRET", ""),

		InternalToken: env("INTERNAL_TOKEN", ""),
		LogLevel:      env("LOG_LEVEL", "info"),
		OTLPEndpoint:  env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
	c.PGReadURL = env("PG_READ_URL", c.PGURL)
	if c.GatewayPublicURL == "" {
		c.GatewayPublicURL = c.GatewayBaseURL
	}
	if c.NIDKey == "" {
		c.NIDKey = c.OTPKey
	}
	var missing []string
	if c.PGURL == "" {
		missing = append(missing, "PG_URL")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// GatewayName is the adapter that serves every payment method locally.
func (c Config) GatewayName() string {
	if len(c.Gateways) > 0 {
		return c.Gateways[0]
	}
	return "fake"
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(env(k, "")); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(env(k, ""), 64); err == nil {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v, err := strconv.ParseBool(env(k, "")); err == nil {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(k, "")); err == nil {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
