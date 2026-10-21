# Load test: sales opening

Replays the rush when a train's sales open, with k6 against the local Docker stack:

- **viewers**: signed-in users polling the seat map (`VIEW_RATE` requests/s, default 600)
- **clickers**: admitted users clicking random seats (`CLICK_RATE` iterations/s, default 300). A successful
  lock is either released again or, for `HOLD_RATIO` (30 %) of them, turned into a hold with passenger
  details and verified with the stack's `OTP_DEV_CODE` (passed as `-e OTP`)

The defaults give about 1,000 requests per second.

## Why the users are prepared outside the API

Login is deliberately slow (argon2id) and limited to 20 per minute per IP, and the local waiting room
admits 50 users per second. That is the wrong bottleneck for a load test, so `test/loadprep` inserts
users straight into Postgres (phones `+880199XXXXXXX`, one shared password hash) and signs their access
and admission tokens with the stack's own keys.

## Run it

From the repository root, with the stack running (`docker compose -p railway-application ... up -d`)
and its secrets loaded into the shell (`set -a; . deploy/.env; set +a`):

```sh
# 1. Copy the signing keys out of the stack (never commit them; see .gitignore)
MSYS_NO_PATHCONV=1 docker cp railway-application-auth-1:/keys ./test/load/keys

# 2. Create 1,000 users with tokens (valid 3 h). Runs inside the Docker network so it reaches
#    the stack's Postgres even if another Postgres is installed on the host.
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o test/load/loadprep ./test/loadprep
MSYS_NO_PATHCONV=1 docker run --rm --network railway-application_default \
  -v "$PWD/test/load:/w" -w /w alpine:3.20 ./loadprep \
  --pg "postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@postgres:5432/$POSTGRES_DB?sslmode=disable" --users 1000 \
  --jwt-key keys/dev-es256.pem --adm-key keys/admission-es256.pem --out users.json

# 3. Pick a journey that is on sale (GET /v1/search ...) and run k6
MSYS_NO_PATHCONV=1 docker run --rm --network railway-application_default \
  -v "$PWD/test/load:/scripts" -e JOURNEY=74220261021 -e OTP=$OTP_DEV_CODE \
  grafana/k6:0.54.0 run /scripts/opening.js

# 4. Check nothing was sold twice
curl -H "X-Internal-Token: $INTERNAL_TOKEN" localhost:9091/internal/v1/journeys/74220261021/invariants
```

`MSYS_NO_PATHCONV=1` is only needed in Git Bash on Windows.

## Settings (`-e NAME=value`)

| Variable | Default | Meaning |
|---|---|---|
| `JOURNEY` | required | Journey ID (must be on sale) |
| `VIEW_RATE` | `600` | Seat-map requests per second |
| `CLICK_RATE` | `300` | Seat clicks per second |
| `DURATION` | `60s` | Length of the run |
| `HOLD_RATIO` | `0.3` | Share of successful locks that go on to a hold + OTP |
| `OTP` | required when `HOLD_RATIO` > 0 | The stack's `OTP_DEV_CODE` |
| `CLASS` | largest pool | Class to compete for |
| `FROM`, `TO` | whole route | Trip stop indexes |
| `BASE` | `http://traefik` | API base URL inside the Docker network |

## Reading the result

| Metric | Meaning |
|---|---|
| `http_req_failed` | 5xx and unexpected codes only; 409/422/429 are normal answers in a seat race |
| `http_req_duration{name:select}` | Seat-lock latency; the specification targets p99 < 250 ms |
| `seat_locked` / `seat_taken` / `sold_out` | Outcomes of clicks |
| `rate_limited` | Users throttled by the 30 clicks/min limit or the cool-down after 10 `SEAT_TAKEN` |
| `rejected_by_rules` | Per-user limits, an existing hold, or a cart for another trip |
| `other_errors` | Anything unexpected; the first occurrence of each code is printed |
| `holds_created`, `otp_verified` | Bookings that reached `PENDING_PAYMENT` |
| `dropped_iterations` | k6 could not start iterations on time (the load generator, not the system) |

Reference run on a laptop (all services on one machine), 60 s, defaults: 911 requests/s, 0 failed
requests, select p95 30 ms / p99 193 ms, seat map p95 7 ms; the 256-seat class sold out, 171 holds were
OTP-verified, and the invariant check reported no double sale.

## Scenario 2: 1,000 users for 10 tickets (`few-seats.js`)

A thundering herd instead of steady traffic. Setup picks `SEATS` seats that are free right now (default
10); each of `USERS` accounts (default 1,000, one k6 VU each) waits for the same start instant and clicks
one of them. On `SEAT_TAKEN` a user tries another of the seats, up to `TRIES` (3) clicks. A winner stops
and books the seat (passenger details + OTP). Teardown checks that every target seat is taken and runs
the invariant check.

```sh
# fresh admission tokens first if earlier runs used them (one admission = one booking): step 2 above
docker run --rm --network railway-application_default -v "$PWD/test/load:/scripts" \
  -e JOURNEY=78720261023 -e OTP=$OTP_DEV_CODE -e INTERNAL_TOKEN grafana/k6:0.54.0 run /scripts/few-seats.js
```

Pass: `winners` is at most `SEATS` (a k6 threshold), the log says `CHECK all 10 target seats are taken`,
and the invariant check says `"ok":true`. Use a train none of the test users holds seats on, or users
answer `ACTIVE_HOLD_EXISTS`.

Reference run on a laptop: exactly **10 winners, 10 bookings, 990 users with nothing** (2,970
`SEAT_TAKEN`), all target seats taken, invariants ok. About 94 % of the losing clicks were refused by the
API's seat mirror without reaching Kafka. Click latency was p50 0.6 s / p99 2 s: the allocator's Kafka
transaction commits took p50 0.6 s on Redpanda's single-core dev container, and 1,000 simultaneous
connections saturated the laptop's CPU. Production brokers commit in tens of milliseconds; measure
latency there, not locally.

### 10,000 users for 100 tickets

Create 10,000 users first (`--users 10000` in step 2), then:

```sh
docker run --rm --network railway-application_default -v "$PWD/test/load:/scripts" \
  -e JOURNEY=70420261024 -e OTP=$OTP_DEV_CODE -e USERS=10000 -e SEATS=100 grafana/k6:0.54.0 run /scripts/few-seats.js
```

Each k6 VU costs memory, so above `MAX_VUS` (default 1,000) users the script keeps 1,000 connections
busy: the first 1,000 users click at the same instant and each next user starts as soon as a connection
is free (all 10,000 arrive within about 15 s).

Reference run on a laptop: **100 winners (100 different users, 100 different seats), 100 bookings, 9,900
users with nothing**, 29,700 `SEAT_TAKEN`, 0 failed requests out of 30,005 (about 1,070 requests/s), all
target seats taken, invariants ok. Click latency p50 0.44 s / p99 1.05 s (laptop limits, see above).

### More tickets than one class or train has

No seeded train has more than 588 seats (all classes together), so for large ticket counts spread the
seats over several trains of the same day and every class:

```sh
# 100,000 users (create them with --users 100000 in step 2; ~18 s, users.json ~80 MB)
docker run --rm --network railway-application_default -v "$PWD/test/load:/scripts" \
  -e JOURNEYS=78720261025,70420261025,70220261025 -e CLASSES=all -e OTP=$OTP_DEV_CODE -e USERS=100000 -e SEATS=1000 \
  grafana/k6:0.54.0 run /scripts/few-seats.js
```

| Variable | Default | Meaning |
|---|---|---|
| `JOURNEY` / `JOURNEYS` | required | One journey ID, or several separated by commas |
| `CLASSES` | `largest` | `largest`, `all`, or one class code |
| `USERS` | `1000` | Users (must not exceed the users in users.json) |
| `SEATS` | `10` | Tickets on offer, picked at random from the free seats |
| `TRIES` | `3` | Clicks per user before giving up |
| `MAX_VUS` | `1000` | Concurrent connections; above this, users queue for a free connection |

## Clean-up

Holds that are not paid expire after 10 minutes and the seats return to sale. The load-test users stay
in the database; `docker compose -p railway-application -f deploy/docker-compose.yml down -v` resets
everything. Delete `test/load/keys/` when you are done.
