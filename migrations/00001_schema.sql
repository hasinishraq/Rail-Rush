-- +goose Up
-- schema_v4.sql (spec Appendix D), verbatim. It runs as one statement inside goose's transaction.
-- +goose StatementBegin
-- =====================================================================
-- Railway Ticket Booking System v4.0 — PostgreSQL 16 schema
--
-- Seat inventory's source of truth is the Kafka inventory-events log,
-- decided by the single-writer allocator. Postgres holds:
--   * reference data (stations, trains, layouts, fares)
--   * users, payments, refunds, OTPs        (system of record, ACID)
--   * bookings and tickets                  (projection of the log)
-- The ticket table carries an EXCLUDE constraint, so even a buggy
-- projector or a manual SQL insert cannot store two active tickets for
-- the same seat on overlapping segments (second line of defence).
--
-- Conventions: UTC TIMESTAMPTZ everywhere; money as BIGINT minor units
-- (poisha) + ISO-4217 currency; TEXT + CHECK instead of ENUM types so
-- values can be added without a type migration.
-- =====================================================================

CREATE EXTENSION IF NOT EXISTS btree_gist;   -- '=' on scalars inside a GiST exclusion

-- =====================================================================
-- 1. Reference data
-- =====================================================================

CREATE TABLE station (
  code        TEXT PRIMARY KEY CHECK (code ~ '^[A-Z]{2,6}$'),     -- 'DHK', 'CTG'
  name        TEXT NOT NULL,                                       -- 'Dhaka (Kamalapur)'
  name_bn     TEXT,
  city        TEXT,
  tz          TEXT NOT NULL DEFAULT 'Asia/Dhaka',
  active      BOOLEAN NOT NULL DEFAULT true
);
CREATE INDEX station_name_prefix ON station (lower(name) text_pattern_ops);

CREATE TABLE seat_class (
  class_code  TEXT PRIMARY KEY CHECK (class_code ~ '^[A-Z_]{2,16}$'), -- S_CHAIR, SNIGDHA, AC_S, AC_B ...
  name        TEXT NOT NULL,
  is_berth    BOOLEAN NOT NULL DEFAULT false,
  sort_order  SMALLINT NOT NULL
);

-- Coach composition template; one layout serves every run of a train.
CREATE TABLE layout (
  layout_id   INT PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  name        TEXT NOT NULL,
  n_seats     INT NOT NULL CHECK (n_seats BETWEEN 1 AND 4000),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE layout_coach (
  layout_id   INT NOT NULL REFERENCES layout,
  coach_code  TEXT NOT NULL,                                      -- 'KHA', 'GA', 'CHA'
  position    SMALLINT NOT NULL,                                  -- order in the rake
  class_code  TEXT NOT NULL REFERENCES seat_class,
  width       SMALLINT NOT NULL,                                  -- drawing grid
  height      SMALLINT NOT NULL,
  PRIMARY KEY (layout_id, coach_code),
  UNIQUE (layout_id, position)
);

CREATE TABLE layout_seat (
  layout_id   INT NOT NULL,
  seat_idx    INT NOT NULL CHECK (seat_idx >= 0),                 -- index into allocator bitmaps
  seat_id     TEXT NOT NULL,                                      -- 'KHA-12'
  coach_code  TEXT NOT NULL,
  seat_no     SMALLINT NOT NULL,
  seat_type   TEXT NOT NULL CHECK (seat_type IN
                ('WINDOW','AISLE','MIDDLE','BERTH_LOWER','BERTH_UPPER')),
  x           SMALLINT NOT NULL,
  y           SMALLINT NOT NULL,
  PRIMARY KEY (layout_id, seat_idx),
  UNIQUE (layout_id, seat_id),
  FOREIGN KEY (layout_id, coach_code) REFERENCES layout_coach (layout_id, coach_code)
);

CREATE TABLE train (
  train_no    TEXT PRIMARY KEY,                                   -- '701'
  name        TEXT NOT NULL,                                      -- 'SUBARNA EXPRESS'
  name_bn     TEXT,
  layout_id   INT NOT NULL REFERENCES layout,
  off_days    SMALLINT[] NOT NULL DEFAULT '{}',                   -- ISO weekday 1..7
  active      BOOLEAN NOT NULL DEFAULT true
);

CREATE TABLE train_stop (                                         -- route; idx 0..n_stops-1
  train_no        TEXT NOT NULL REFERENCES train,
  idx             SMALLINT NOT NULL CHECK (idx BETWEEN 0 AND 64),
  station_code    TEXT NOT NULL REFERENCES station,
  arr_offset_min  INT,                                            -- NULL at origin
  dep_offset_min  INT,                                            -- NULL at terminus
  km              INT NOT NULL CHECK (km >= 0),
  PRIMARY KEY (train_no, idx),
  UNIQUE (train_no, station_code)
);
CREATE INDEX train_stop_station ON train_stop (station_code, train_no);

CREATE TABLE quota_plan (
  quota_plan_id INT PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  name          TEXT NOT NULL
);

CREATE TABLE quota_plan_seat (                                    -- initial quota of each seat
  quota_plan_id INT NOT NULL REFERENCES quota_plan,
  seat_idx      INT NOT NULL,
  quota         TEXT NOT NULL CHECK (quota ~ '^[A-Z_]{2,16}$'),   -- ONLINE, COUNTER, VIP, STAFF ...
  PRIMARY KEY (quota_plan_id, seat_idx)
);

-- Fares are per station pair and class (Bangladesh Railway fare chart).
CREATE TABLE fare (
  class_code    TEXT NOT NULL REFERENCES seat_class,
  from_station  TEXT NOT NULL REFERENCES station,
  to_station    TEXT NOT NULL REFERENCES station,
  valid_from    DATE NOT NULL,
  amount_minor  BIGINT NOT NULL CHECK (amount_minor > 0),         -- base fare incl. VAT, poisha
  currency      TEXT NOT NULL DEFAULT 'BDT',
  PRIMARY KEY (class_code, from_station, to_station, valid_from),
  CHECK (from_station <> to_station)
);

CREATE TABLE charge_rule (                                        -- online service charge etc.
  charge_code   TEXT PRIMARY KEY,                                 -- 'SERVICE'
  per_ticket_minor BIGINT NOT NULL CHECK (per_ticket_minor >= 0),
  valid_from    DATE NOT NULL
);

-- =====================================================================
-- 2. Journeys (one train on one run date)
-- =====================================================================

CREATE TABLE journey (
  journey_id      BIGINT PRIMARY KEY,
  train_no        TEXT NOT NULL REFERENCES train,
  run_date        DATE NOT NULL,
  departs_at      TIMESTAMPTZ NOT NULL,
  n_segments      SMALLINT NOT NULL CHECK (n_segments BETWEEN 1 AND 64),
  layout_id       INT NOT NULL REFERENCES layout,
  quota_plan_id   INT NOT NULL REFERENCES quota_plan,
  sales_close_at  TIMESTAMPTZ NOT NULL,                           -- online sales stop
  status          TEXT NOT NULL DEFAULT 'SCHEDULED'
                    CHECK (status IN ('SCHEDULED','CLOSED','DEPARTED','CANCELLED')),
  UNIQUE (train_no, run_date),
  CHECK (sales_close_at <= departs_at)
);
CREATE INDEX journey_run_date ON journey (run_date);

CREATE TABLE journey_pool (
  journey_id          BIGINT NOT NULL REFERENCES journey,
  class_code          TEXT NOT NULL REFERENCES seat_class,
  quota               TEXT NOT NULL,
  opens_at            TIMESTAMPTZ NOT NULL,                       -- e.g. 08:00 Asia/Dhaka, stored UTC
  max_seats_per_user  SMALLINT NOT NULL DEFAULT 4 CHECK (max_seats_per_user BETWEEN 1 AND 6),
  PRIMARY KEY (journey_id, class_code, quota)
);
CREATE INDEX journey_pool_opens ON journey_pool (opens_at);

CREATE TABLE journey_snapshot (                                   -- allocator restart point
  journey_id      BIGINT PRIMARY KEY REFERENCES journey,
  kafka_partition INT NOT NULL,
  kafka_offset    BIGINT NOT NULL,
  state           BYTEA NOT NULL,                                 -- protobuf JourneySnapshot
  taken_at        TIMESTAMPTZ NOT NULL
);

-- =====================================================================
-- 3. Users and auth
-- =====================================================================

CREATE TABLE app_user (
  user_id       BIGINT PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  phone         TEXT NOT NULL UNIQUE CHECK (phone ~ '^\+8801[3-9][0-9]{8}$'),
  email         TEXT,
  name          TEXT NOT NULL,
  nid_hash      BYTEA UNIQUE,                                     -- HMAC(NID); one account per NID
  password_hash TEXT NOT NULL,                                    -- argon2id PHC string
  verified      BOOLEAN NOT NULL DEFAULT false,
  role          TEXT NOT NULL DEFAULT 'passenger'
                  CHECK (role IN ('passenger','counter','agent','admin')),
  status        TEXT NOT NULL DEFAULT 'ACTIVE'
                  CHECK (status IN ('ACTIVE','LOCKED','BANNED')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX app_user_email ON app_user (lower(email)) WHERE email IS NOT NULL;

CREATE TABLE refresh_token (
  token_hash  BYTEA PRIMARY KEY,                                  -- sha256(token)
  family_id   UUID NOT NULL,
  user_id     BIGINT NOT NULL REFERENCES app_user,
  device_id   TEXT,
  expires_at  TIMESTAMPTZ NOT NULL,
  used_at     TIMESTAMPTZ,
  revoked_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_token_family ON refresh_token (family_id);
CREATE INDEX refresh_token_user   ON refresh_token (user_id);

CREATE TABLE saved_passenger (
  id              BIGINT PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  user_id         BIGINT NOT NULL REFERENCES app_user,
  name            TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 40),
  gender          TEXT NOT NULL CHECK (gender IN ('MALE','FEMALE','OTHER')),
  passenger_type  TEXT NOT NULL DEFAULT 'ADULT' CHECK (passenger_type IN ('ADULT','CHILD')),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX saved_passenger_user ON saved_passenger (user_id);

-- =====================================================================
-- 4. Bookings and tickets (projection of inventory-events)
--    Partitioned monthly by run_date. PNRs are globally unique by
--    construction (Snowflake-style generator); the primary key includes
--    run_date because Postgres requires the partition key in it.
-- =====================================================================

CREATE TABLE booking (
  run_date          DATE NOT NULL,
  pnr               TEXT NOT NULL CHECK (pnr ~ '^[0-9]{10}$'),
  journey_id        BIGINT NOT NULL,
  user_id           BIGINT NOT NULL,
  from_idx          SMALLINT NOT NULL,
  to_idx            SMALLINT NOT NULL,
  class_code        TEXT NOT NULL,
  quota             TEXT NOT NULL,
  status            TEXT NOT NULL CHECK (status IN (
                      'PENDING_PAYMENT','CONFIRMED','PARTIALLY_CANCELLED',
                      'CANCELLED','EXPIRED','CONFIRM_FAILED')),
  hold_expires_at   TIMESTAMPTZ,                                 -- OTP state lives in booking_otp
  amount_minor      BIGINT NOT NULL CHECK (amount_minor >= 0),
  currency          TEXT NOT NULL DEFAULT 'BDT',
  idem_key          TEXT NOT NULL,                                -- "{user_id}:{Idempotency-Key}"
  created_at        TIMESTAMPTZ NOT NULL,
  updated_at        TIMESTAMPTZ NOT NULL,
  last_event_offset BIGINT NOT NULL,                              -- ignore older events on replay
  PRIMARY KEY (run_date, pnr),
  CHECK (from_idx < to_idx)
) PARTITION BY RANGE (run_date);
CREATE INDEX booking_pnr     ON booking (pnr);
CREATE INDEX booking_user    ON booking (user_id, created_at DESC);
CREATE INDEX booking_journey ON booking (journey_id, status);

CREATE TABLE ticket (                                             -- one row per passenger seat
  run_date      DATE NOT NULL,
  pnr           TEXT NOT NULL,
  seq           SMALLINT NOT NULL CHECK (seq BETWEEN 1 AND 6),
  journey_id    BIGINT NOT NULL,
  user_id       BIGINT NOT NULL,
  seat_idx      INT NOT NULL,
  seat_id       TEXT NOT NULL,                                    -- 'KHA-12'
  coach_code    TEXT NOT NULL,
  from_idx      SMALLINT NOT NULL,
  to_idx        SMALLINT NOT NULL,
  segs          INT4RANGE GENERATED ALWAYS AS (int4range(from_idx, to_idx)) STORED,
  passenger_name TEXT NOT NULL,
  gender        TEXT NOT NULL CHECK (gender IN ('MALE','FEMALE','OTHER')),
  passenger_type TEXT NOT NULL DEFAULT 'ADULT' CHECK (passenger_type IN ('ADULT','CHILD')),
  fare_minor    BIGINT NOT NULL CHECK (fare_minor >= 0),
  status        TEXT NOT NULL CHECK (status IN
                  ('HELD','ISSUED','CANCELLED','EXPIRED','FAILED')),
  created_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (run_date, pnr, seq),
  CHECK (from_idx < to_idx)
) PARTITION BY RANGE (run_date);
CREATE INDEX ticket_journey_seat ON ticket (journey_id, seat_idx);
CREATE INDEX ticket_user         ON ticket (user_id, run_date);
-- The no-double-sell EXCLUDE constraint is added per partition by
-- create_month_partitions(): PostgreSQL 16 cannot declare it on a
-- partitioned parent. A journey lives in exactly one partition, so a
-- per-partition constraint is just as strong.

-- Per-user purchase counters for the "4 tickets per user per day" rule
-- (OrderLimitExceeded across trains). Maintained by the projector;
-- mirrored to Redis for the fast API check.
CREATE TABLE purchase_counter (
  user_id       BIGINT NOT NULL,
  run_date      DATE NOT NULL,
  seats_issued  SMALLINT NOT NULL DEFAULT 0 CHECK (seats_issued >= 0),
  PRIMARY KEY (user_id, run_date)
);

-- OTP sent at passenger-details time (API-owned, not allocator state).
CREATE TABLE booking_otp (
  pnr           TEXT PRIMARY KEY,
  user_id       BIGINT NOT NULL,
  code_hash     BYTEA NOT NULL,                                   -- HMAC(code), never the code
  sent_to       TEXT NOT NULL,                                    -- phone
  attempts      SMALLINT NOT NULL DEFAULT 0,
  max_attempts  SMALLINT NOT NULL DEFAULT 5,
  resends       SMALLINT NOT NULL DEFAULT 0,
  expires_at    TIMESTAMPTZ NOT NULL,
  verified_at   TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (attempts <= max_attempts)
);
CREATE INDEX booking_otp_expiry ON booking_otp (expires_at) WHERE verified_at IS NULL;

-- =====================================================================
-- 5. Money (system of record)
--    No FK from payment to booking: payments are created from the
--    allocator's hold:{pnr} in Redis before the projection catches up.
-- =====================================================================

CREATE TABLE payment (
  payment_id        UUID PRIMARY KEY,
  pnr               TEXT NOT NULL,
  run_date          DATE NOT NULL,
  journey_id        BIGINT NOT NULL,
  user_id           BIGINT NOT NULL REFERENCES app_user,
  method            TEXT NOT NULL CHECK (method IN
                      ('BKASH','NAGAD','ROCKET','UPAY','VISA','MASTERCARD','NEXUS')),
  gateway           TEXT NOT NULL,                                -- 'bkash', 'sslcommerz', ...
  gateway_order_id  TEXT UNIQUE,
  redirect_used_at  TIMESTAMPTZ,                                  -- payment link is single-use
  status            TEXT NOT NULL CHECK (status IN
                      ('INITIATED','CAPTURED','FAILED','REFUNDED','PARTIALLY_REFUNDED')),
  amount_minor      BIGINT NOT NULL CHECK (amount_minor > 0),
  currency          TEXT NOT NULL DEFAULT 'BDT',
  captured_at       TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (status <> 'CAPTURED' OR captured_at IS NOT NULL)
);
CREATE UNIQUE INDEX one_live_payment_per_pnr ON payment (pnr)
  WHERE status IN ('INITIATED','CAPTURED');
CREATE INDEX payment_reconcile ON payment (status, created_at);
CREATE INDEX payment_user      ON payment (user_id, created_at DESC);

CREATE TABLE refund (
  refund_id         UUID PRIMARY KEY,
  payment_id        UUID NOT NULL REFERENCES payment,
  pnr               TEXT NOT NULL,
  reason            TEXT NOT NULL CHECK (reason IN
                      ('CANCELLED','CONFIRM_FAILED','SALES_CLOSED','AMOUNT_MISMATCH','DUPLICATE_CAPTURE')),
  idem_key          TEXT NOT NULL UNIQUE,                         -- pnr + cancel_id | event_id
  amount_minor      BIGINT NOT NULL CHECK (amount_minor > 0),
  status            TEXT NOT NULL CHECK (status IN ('INITIATED','SUCCEEDED','FAILED')),
  gateway_refund_id TEXT,
  attempts          SMALLINT NOT NULL DEFAULT 0,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refund_pending ON refund (status, updated_at) WHERE status <> 'SUCCEEDED';

CREATE TABLE webhook_event (                                      -- dedupe gateway deliveries
  gateway     TEXT NOT NULL,
  event_id    TEXT NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (gateway, event_id)
);

-- =====================================================================
-- 6. Plumbing
-- =====================================================================

CREATE TABLE outbox (
  id          BIGINT PRIMARY KEY GENERATED ALWAYS AS IDENTITY,
  topic       TEXT NOT NULL,
  msg_key     TEXT NOT NULL,
  payload     BYTEA NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  sent_at     TIMESTAMPTZ
);
CREATE INDEX outbox_unsent ON outbox (id) WHERE sent_at IS NULL;

CREATE TABLE idempotency_key (                                    -- API-level, non-allocator calls
  user_id      BIGINT NOT NULL,
  endpoint     TEXT NOT NULL,
  key          UUID NOT NULL,
  request_hash BYTEA NOT NULL,
  status_code  SMALLINT,
  response     BYTEA,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, endpoint, key)
);
CREATE INDEX idempotency_key_age ON idempotency_key (created_at);

CREATE TABLE projector_offset (
  consumer     TEXT NOT NULL,
  part         INT NOT NULL,
  last_offset  BIGINT NOT NULL,
  PRIMARY KEY (consumer, part)
);

CREATE TABLE scheduler_job (
  journey_id  BIGINT NOT NULL REFERENCES journey,
  kind        TEXT NOT NULL CHECK (kind IN ('WARM','OPEN','CLOSE')),
  due_at      TIMESTAMPTZ NOT NULL,
  sent_at     TIMESTAMPTZ,
  PRIMARY KEY (journey_id, kind, due_at)
);
CREATE INDEX scheduler_job_due ON scheduler_job (due_at) WHERE sent_at IS NULL;

-- =====================================================================
-- 7. Partition management (run monthly from the scheduler; idempotent)
-- =====================================================================

CREATE OR REPLACE FUNCTION create_month_partitions(p_month DATE)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  m_start DATE := date_trunc('month', p_month)::date;
  m_end   DATE := (date_trunc('month', p_month) + interval '1 month')::date;
  sfx     TEXT := to_char(m_start, 'YYYY_MM');
BEGIN
  EXECUTE format('CREATE TABLE IF NOT EXISTS booking_%s PARTITION OF booking
                  FOR VALUES FROM (%L) TO (%L)', sfx, m_start, m_end);
  EXECUTE format('CREATE TABLE IF NOT EXISTS ticket_%s PARTITION OF ticket
                  FOR VALUES FROM (%L) TO (%L)', sfx, m_start, m_end);

  -- No double sell: two HELD/ISSUED tickets may never share a seat on
  -- overlapping segments of the same journey.
  IF NOT EXISTS (SELECT 1 FROM pg_constraint
                 WHERE conname = format('ticket_%s_no_overlap', sfx)) THEN
    EXECUTE format('ALTER TABLE ticket_%s ADD CONSTRAINT ticket_%s_no_overlap
                    EXCLUDE USING gist (journey_id WITH =, seat_idx WITH =, segs WITH &&)
                    WHERE (status IN (''HELD'',''ISSUED''))', sfx, sfx);
  END IF;
END $$;

-- Current month plus the 120-day advance-booking horizon.
DO $$
BEGIN
  FOR m IN 0..5 LOOP
    PERFORM create_month_partitions((date_trunc('month', now()) + make_interval(months => m))::date);
  END LOOP;
END $$;

-- =====================================================================
-- 8. Invariant checks (run after every window; expected: zero rows)
-- =====================================================================

-- Every captured payment ends as an issued ticket or with a refund.
CREATE VIEW v_money_without_ticket AS
SELECT p.payment_id, p.pnr, b.status
FROM payment p
LEFT JOIN booking b ON b.pnr = p.pnr AND b.run_date = p.run_date
WHERE p.status = 'CAPTURED'
  AND p.captured_at < now() - interval '30 minutes'
  AND (b.status IS NULL
       OR b.status IN ('PENDING_PAYMENT','EXPIRED')
       OR (b.status = 'CONFIRM_FAILED'
           AND NOT EXISTS (SELECT 1 FROM refund r WHERE r.pnr = p.pnr)));

-- Per-user limit breaches (should be impossible; alarms if not).
CREATE VIEW v_purchase_limit_breach AS
SELECT t.user_id, t.journey_id, count(*) AS seats
FROM ticket t
WHERE t.status = 'ISSUED'
GROUP BY t.user_id, t.journey_id
HAVING count(*) > 4;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS v_purchase_limit_breach, v_money_without_ticket;
DROP FUNCTION IF EXISTS create_month_partitions(date);
DROP TABLE IF EXISTS scheduler_job, projector_offset, idempotency_key, outbox, webhook_event, refund, payment,
  booking_otp, purchase_counter, ticket, booking, saved_passenger, refresh_token, app_user, journey_snapshot,
  journey_pool, journey, charge_rule, fare, quota_plan_seat, quota_plan, train_stop, train, layout_seat,
  layout_coach, layout, seat_class, station CASCADE;
-- +goose StatementEnd
