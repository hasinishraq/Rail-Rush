// Package metrics declares the Prometheus metrics behind the alerts in §24.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "HTTP requests by route and status.",
	}, []string{"route", "status"})
	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_request_duration_seconds", Help: "HTTP latency by route.",
		Buckets: []float64{.005, .01, .025, .05, .1, .2, .3, .5, 1, 2, 5},
	}, []string{"route"})
	APIErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "api_errors_total", Help: "API error responses by code.",
	}, []string{"code"})
	CommandResults = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "command_results_total", Help: "Select / release / hold / cancel outcomes (event kind or rejection code; FAST_* = answered without Kafka).",
	}, []string{"command", "result"})
	DecisionSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "command_decision_seconds", Help: "Produce-to-result latency seen by the API, by command.",
		Buckets: []float64{.01, .025, .05, .1, .15, .25, .5, 1, 2},
	}, []string{"command"})

	AllocatorCommitFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "allocator_commit_failures_total", Help: "Kafka transaction commits that failed or aborted.",
	})
	AllocatorCommitSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "allocator_commit_seconds", Help: "Kafka transaction commit latency.",
		Buckets: []float64{.002, .005, .01, .02, .05, .1, .25, .5, 1},
	})
	AllocatorEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "allocator_events_total", Help: "Events committed by kind.",
	}, []string{"kind"})
	AllocatorLagMS = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "allocator_partition_lag_ms", Help: "Age of the newest command processed per partition.",
	}, []string{"partition"})
	AllocatorResident = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "allocator_resident_journeys", Help: "Journeys held in memory.",
	})

	LiveConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "live_connections", Help: "Open seat map SSE connections on this pod.",
	})
	LiveDeltaAge = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "live_delta_age_seconds", Help: "Age of seat deltas when they reach the live pod.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 5},
	})
	LiveSlowDisconnects = promauto.NewCounter(prometheus.CounterOpts{
		Name: "live_slow_disconnects_total", Help: "SSE clients dropped because their buffer was full.",
	})
	MirrorJourneys = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "seatmirror_journeys", Help: "Hot journeys mirrored in this API pod.",
	})

	OTPSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otp_sent_total", Help: "OTPs issued, by kind (issue, resend).",
	}, []string{"kind"})
	OTPVerify = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "otp_verify_total", Help: "OTP verification outcomes.",
	}, []string{"result"})
	SMSSent = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_sent_total", Help: "SMS deliveries by provider and outcome.",
	}, []string{"provider", "result"})

	OutboxOldestSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "outbox_oldest_unsent_seconds", Help: "Age of the oldest unsent outbox row.",
	})
	ProjectorEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "projector_events_total", Help: "Events projected by kind.",
	}, []string{"kind"})
	ProjectorExcludeViolations = promauto.NewCounter(prometheus.CounterOpts{
		Name: "projector_exclude_violations_total", Help: "Ticket rows rejected by the no-overlap constraint (page).",
	})
	PaymentsStuck = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "payments_initiated_stuck", Help: "Payments INITIATED for more than 15 minutes.",
	})
	RefundsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "refunds_not_succeeded", Help: "Refunds not SUCCEEDED.",
	})
	WebhookSignatureFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "webhook_signature_failures_total", Help: "Webhook deliveries with a bad signature.",
	})
	RefreshReused = promauto.NewCounter(prometheus.CounterOpts{
		Name: "auth_refresh_reused_total", Help: "Refresh tokens replayed (possible theft).",
	})
)
