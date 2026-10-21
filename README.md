# RailRush

> 📖 **Full documentation: <https://hasinishraq.github.io/Rail-Rush-Documentation/>**
> Architecture, API reference, operations guide, and a narrated architecture video.

**A high-concurrency, real-time train ticketing system**, built around Bangladesh Railway's intercity
network (a personal project, not affiliated with Bangladesh Railway).

Every viewer sees a live seat map. Each click locks a seat in real time; passenger details turn the
locked seats into a 10-minute hold confirmed by an SMS one-time code, and payment issues a signed
e-ticket. The hard part is the moment a train's sales open, when thousands of people click the same
seats at once. RailRush never sells a seat twice.

## Main features

- **No double sales.** One allocator worker owns each journey and decides every seat in memory. Seat
  availability per trip segment is a bitmap, so overlapping trips can't share a seat, and a Postgres
  `EXCLUDE` constraint backs this up.
- **Event sourcing.** Every seat decision is an event in Kafka. The Postgres projection, seat maps, live
  updates and notifications are all derived from those events.
- **Scalability.** Requests are partitioned by journey across 256 Kafka partitions. The API is
  stateless, and the API, live and allocator services scale out as replicas. Overloaded pods answer
  `429` instead of falling over.
- **Waiting room.** Users are admitted at a fixed rate with ES256-signed admission tokens, which absorbs
  the spike when sales open.
- **Saga workflows.** A booking moves from seat selection to hold, OTP, payment and confirmation through
  timer-driven steps. Idle carts are released, unpaid holds expire, a late payment gets the nearest free
  seat, and a payment with no seat left is refunded in full.
- **Reliable payments.** The outbox pattern, signed webhooks and a reconciler make sure no payment is
  lost or applied twice.
- **Crash recovery.** After a crash, allocators rebuild each journey from snapshots plus the event log.
- **Real-time updates.** Seat changes go out to every viewer as server-sent events (SSE).
- **Security.** Argon2id passwords, ES256 JWTs with refresh-token reuse detection, Cloudflare Turnstile
  on sign-up and login, per-IP and per-user rate limits, and signed QR codes on e-tickets.
- **Observability.** Prometheus metrics, Grafana dashboards, and logs from every container in Loki.
- **Bilingual.** The web app and PDF e-tickets come in English and Bangla.

## Technology

| | |
|---|---|
| Languages | Go, TypeScript, SQL, Protocol Buffers |
| Backend | Go: one binary run in several roles, plus the stateful allocator |
| Messaging | Kafka (Redpanda), Protocol Buffers |
| Storage | PostgreSQL 16, Redis 7 |
| Frontend | React 18, TypeScript, Vite |
| Edge | Traefik, nginx |
| Ops | Docker Compose, Prometheus, Grafana, Loki, Alloy |
| Testing | Go tests with property-based testing (rapid), k6 load tests |

## How it works

```
browser ─▶ nginx ─▶ Traefik ─┬─▶ auth     login, tokens, Cloudflare Turnstile
                             ├─▶ search   trains, layouts, seat maps
                             ├─▶ api      seats, holds, OTP, payments ──▶ Kafka (booking-requests)
                             └─▶ live     seat updates over SSE                      │
                                                                                     ▼
                         Postgres ◀── projector ◀── Kafka (inventory-events) ◀── allocator
                                                                                (one writer per journey)
```

## Load-tested

With [k6](test/load/README.md) against the whole stack on one laptop:

- **Sales opening**, 60 s: 911 requests/s, 0 failed requests, seat-lock p99 193 ms; the class sold out
  with no double sale.
- **10,000 users racing for 100 seats**: exactly 100 winners on 100 different seats, 0 failed requests,
  invariant check clean.

## Run it

Requires Docker.

```sh
cp deploy/.env.example deploy/.env    # then fill in the empty secrets (openssl rand -hex 32)
docker compose -p railway-application -f deploy/docker-compose.yml up -d --build
```

Open the web app at <http://localhost:8088>. Grafana is on <http://localhost:3000>.

## Documentation

See the full documentation at <https://hasinishraq.github.io/Rail-Rush-Documentation/>.
