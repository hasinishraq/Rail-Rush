// k6 load test: the rush when a train's sales open.
//
// Two scenarios run together against one journey:
//   viewers  - signed-in users polling the seat map           (VIEW_RATE requests/s)
//   clickers - admitted users clicking random seats; a lock is (CLICK_RATE iterations/s)
//              either held (passenger details + OTP_DEV_CODE) or released again
//
// Users and tokens come from test/loadprep (users.json). See test/load/README.md.
import http from "k6/http";
import { check } from "k6";
import { SharedArray } from "k6/data";
import { Counter, Trend } from "k6/metrics";

const BASE = __ENV.BASE || "http://traefik";
const JOURNEY = __ENV.JOURNEY;
const VIEW_RATE = Number(__ENV.VIEW_RATE || 600);
const CLICK_RATE = Number(__ENV.CLICK_RATE || 300);
const DURATION = __ENV.DURATION || "60s";
const HOLD_RATIO = Number(__ENV.HOLD_RATIO || 0.3); // share of successful locks that go on to a hold
const OTP = __ENV.OTP; // the stack's OTP_DEV_CODE
if (HOLD_RATIO > 0 && !OTP) throw new Error("set -e OTP=<OTP_DEV_CODE from deploy/.env>, or -e HOLD_RATIO=0");

const users = new SharedArray("users", () => JSON.parse(open("./users.json")));

const locked = new Counter("seat_locked");
const taken = new Counter("seat_taken");
const soldOut = new Counter("sold_out");
const limited = new Counter("rate_limited");
const overloaded = new Counter("overloaded_or_unavailable");
const ruleRejected = new Counter("rejected_by_rules"); // per-user limits, existing hold, cart for another trip
const otherErr = new Counter("other_errors");
const RULE_CODES = ["ACTIVE_HOLD_EXISTS", "SELECTION_LIMIT", "ORDER_LIMIT_EXCEEDED", "CART_CONFLICT", "ADMISSION_USED"];
const seenOther = {};
const holds = new Counter("holds_created");
const verified = new Counter("otp_verified");
const lockLatency = new Trend("lock_decision_ms", true);

// 409 / 422 / 429 are normal answers in a seat race; only 5xx and unexpected codes count as failures.
http.setResponseCallback(http.expectedStatuses(200, 201, 202, 204, 403, 404, 409, 422, 429));

export const options = {
  scenarios: {
    viewers: {
      executor: "constant-arrival-rate", exec: "viewer", rate: VIEW_RATE, timeUnit: "1s",
      duration: DURATION, preAllocatedVUs: 50, maxVUs: 400,
    },
    clickers: {
      executor: "constant-arrival-rate", exec: "clicker", rate: CLICK_RATE, timeUnit: "1s",
      duration: DURATION, preAllocatedVUs: 100, maxVUs: 800,
    },
  },
  thresholds: {
    "http_req_failed": ["rate<0.01"],
    "http_req_duration{name:seatmap}": ["p(95)<200"],
    "http_req_duration{name:select}": ["p(95)<250"],   // spec target: decision p99 < 250 ms
    "http_req_duration{name:hold}": ["p(95)<500"],
  },
  summaryTrendStats: ["avg", "med", "p(90)", "p(95)", "p(99)", "max"],
};

const auth = (u, extra) => ({ headers: Object.assign({ Authorization: `Bearer ${u.token}` }, extra || {}) });
const pickUser = () => users[Math.floor(Math.random() * users.length)];

export function setup() {
  if (!JOURNEY) throw new Error("set JOURNEY=<journey_id>");
  const u = users[0];
  const j = http.get(`${BASE}/v1/journeys/${JOURNEY}`, auth(u)).json();
  const pools = j.pools.filter((p) => p.quota === "ONLINE");
  const cls = __ENV.CLASS || pools.sort((a, b) => b.seats - a.seats)[0].class_code;
  const layout = http.get(`${BASE}/v1/layouts/${j.layout_id}`, auth(u)).json();
  const seats = [];
  for (const c of layout.coaches) if (c.class_code === cls) for (const s of c.seats) seats.push(s.seat_id);
  const from = Number(__ENV.FROM || 0);
  const to = Number(__ENV.TO || j.route.length - 1);
  const sm = http.get(`${BASE}/v1/journeys/${JOURNEY}/seatmap`, auth(u));
  if (sm.status !== 200) throw new Error(`seat map ${sm.status}: is the train on sale? ${sm.body}`);
  console.log(`journey ${JOURNEY} ${j.train_name}: class ${cls}, ${seats.length} seats, trip ${from}->${to}, ${users.length} users`);
  return { cls, seats, from, to };
}

export function viewer() {
  const res = http.get(`${BASE}/v1/journeys/${JOURNEY}/seatmap`, Object.assign(auth(pickUser()), { tags: { name: "seatmap" } }));
  check(res, { "seat map 200": (r) => r.status === 200 });
}

export function clicker(d) {
  const u = pickUser();
  const seat = d.seats[Math.floor(Math.random() * d.seats.length)];
  const hdr = { "Content-Type": "application/json", "X-Admission-Token": u.admission };
  const res = http.put(`${BASE}/v1/journeys/${JOURNEY}/selection/${seat}`,
    JSON.stringify({ from_idx: d.from, to_idx: d.to, class_code: d.cls }),
    Object.assign(auth(u, hdr), { tags: { name: "select" } }));
  lockLatency.add(res.timings.duration);
  const code = res.status >= 400 ? (res.json("error.code") || String(res.status)) : "";
  if (res.status === 200) locked.add(1);
  else if (code === "SEAT_TAKEN") taken.add(1);
  else if (code === "SOLD_OUT") soldOut.add(1);
  else if (code === "RATE_LIMITED") limited.add(1);
  else if (code === "OVERLOADED" || code === "LOG_UNAVAILABLE" || res.status >= 500) overloaded.add(1);
  else if (RULE_CODES.includes(code)) ruleRejected.add(1);
  else if (res.status !== 202) {
    otherErr.add(1, { code });
    if (!seenOther[code]) { seenOther[code] = true; console.warn(`unexpected select answer ${res.status} ${code}`); }
  }
  if (res.status !== 200) return;

  const cart = res.json();
  if (Math.random() >= HOLD_RATIO) {
    // Changed their mind: release, so the seat goes back to the race.
    http.del(`${BASE}/v1/journeys/${JOURNEY}/selection/*`, null,
      Object.assign(auth(u, { "X-Admission-Token": u.admission }), { tags: { name: "release" } }));
    return;
  }
  const pax = cart.seats.map((_, i) => ({ name: `Load User ${u.user_id}-${i + 1}`, gender: "MALE", type: "ADULT" }));
  const h = http.post(`${BASE}/v1/holds`, JSON.stringify({ journey_id: Number(JOURNEY), passengers: pax }),
    Object.assign(auth(u, { "Content-Type": "application/json", "X-Admission-Token": u.admission,
      "Idempotency-Key": uuid() }), { tags: { name: "hold" } }));
  if (h.status !== 201) return;
  holds.add(1);
  const v = http.post(`${BASE}/v1/holds/${h.json("pnr")}/otp/verify`, JSON.stringify({ otp: OTP }),
    Object.assign(auth(u, { "Content-Type": "application/json" }), { tags: { name: "otp" } }));
  if (v.status === 200) verified.add(1);
}

function uuid() {
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === "x" ? r : (r & 0x3) | 0x8).toString(16);
  });
}
