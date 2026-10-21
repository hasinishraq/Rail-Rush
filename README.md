# RailRush

**A high-concurrency, real-time train ticketing system**, built around Bangladesh Railway's intercity
network (a personal project, not affiliated with Bangladesh Railway).

Every viewer sees a live seat map. Each click locks a seat in real time; passenger details turn the
locked seats into a 10-minute hold confirmed by an SMS one-time code, and payment issues a signed
e-ticket. The hard part is the moment a train's sales open, when thousands of people click the same
seats at once. RailRush never sells a seat twice.

## How it works

Every seat decision is made by **a single writer per journey**, in memory, and recorded as an event in
Kafka. Everything else (the Postgres projection, seat maps, live updates, notifications) is derived from
those events.

```
browser ─▶ nginx ─▶ Traefik ─┬─▶ auth     login, tokens, Cloudflare Turnstile
                             ├─▶ search   trains, layouts, seat maps
                             ├─▶ api      seats, holds, OTP, payments ──▶ Kafka (booking-requests)
                             └─▶ live     seat updates over SSE                      │
                                                                                     ▼
                         Postgres ◀── projector ◀── Kafka (inventory-events) ◀── allocator
                                                                                (one writer per journey)
```

- **Event-sourced seat allocator.** Requests are partitioned by journey across 256 Kafka partitions, so
  one allocator worker decides every seat of a journey. Seat availability per trip segment is a bitmap,
  so overlapping trips can't share a seat. A Postgres `EXCLUDE` constraint backs this up.
- **Real time.** Seat changes go out as server-sent events to every viewer.
- **Built for the rush.** A waiting room admits users at a fixed rate with ES256-signed admission tokens.
  Stateless API, live and allocator replicas scale out, and saturated pods answer `429` instead of
  falling over.
- **Fails safely.** Allocators rebuild journeys from snapshots plus the event log after a crash. Payments
  use the outbox pattern, signed webhooks and a reconciler.
- **Security.** Argon2id passwords, ES256 JWTs with refresh-token reuse detection, Cloudflare Turnstile
  on sign-up and login, per-IP and per-user rate limits, and signed QR codes on e-tickets.
- **Observability.** Prometheus metrics, Grafana dashboards, and logs from every container in Loki.

## Load-tested

With [k6](test/load/README.md) against the whole stack on one laptop:

- **Sales opening**, 60 s: 911 requests/s, 0 failed requests, seat-lock p99 193 ms; the class sold out
  with no double sale.
- **10,000 users racing for 100 seats**: exactly 100 winners on 100 different seats, 0 failed requests,
  invariant check clean.

## Tech stack

| | |
|---|---|
| Backend | Go: one binary run in several roles, plus the stateful allocator |
| Messaging | Kafka (Redpanda), Protocol Buffers |
| Storage | PostgreSQL 16, Redis 7 |
| Frontend | React 18, TypeScript, Vite; English and Bangla |
| Edge | Traefik, nginx |
| Ops | Docker Compose, Prometheus, Grafana, Loki, Alloy |

## Run it

Requires Docker.

```sh
cp deploy/.env.example deploy/.env    # then fill in the empty secrets (openssl rand -hex 32)
docker compose -p railway-application -f deploy/docker-compose.yml up -d --build
```

Open the web app at <http://localhost:8088>. Grafana is on <http://localhost:3000>.

## Documentation

The full docs (architecture, API reference, operations, and a narrated architecture video) are in
[`documentation/`](documentation/README.md). Start with
[local setup](documentation/docs/getting-started/local-setup.md) and the
[architecture overview](documentation/docs/architecture/index.md). The API can also be explored with the
[Postman collection](documentation/postman/).
