// k6 load test: many users, very few tickets (default 1,000 users for 10 seats).
//
// Setup picks SEATS seats that are free right now, on one train (JOURNEY) or spread over several
// (JOURNEYS=a,b,c), in the largest class or in every class (CLASSES=all). Every user waits for the
// same start instant, then clicks one of those seats; on SEAT_TAKEN they try another, up to TRIES
// clicks. A user who gets a seat stops and books it (passenger details + OTP), so it stays taken.
// Teardown checks the seats on the seat maps and runs the allocator invariant check per train.
//
// Pass: winners <= SEATS, every target seat taken, invariants ok. See test/load/README.md.
import http from "k6/http";
import { sleep } from "k6";
import encoding from "k6/encoding";
import exec from "k6/execution";
import { SharedArray } from "k6/data";
import { Counter, Trend } from "k6/metrics";

const BASE = __ENV.BASE || "http://traefik";
const INTERNAL = __ENV.INTERNAL || "http://auth:9091";
const INTERNAL_TOKEN = __ENV.INTERNAL_TOKEN || "";
const JOURNEYS = (__ENV.JOURNEYS || __ENV.JOURNEY || "").split(",").map((s) => s.trim()).filter(Boolean);
const CLASSES = __ENV.CLASSES || __ENV.CLASS || "largest"; // largest | all | a class code
const SEATS = Number(__ENV.SEATS || 10);
const USERS = Number(__ENV.USERS || 1000);
const TRIES = Number(__ENV.TRIES || 3);
// Each k6 VU costs memory. Up to MAX_VUS users every user is its own VU and all click at the same
// instant; above that, MAX_VUS connections work through the users: the first MAX_VUS go at once and
// each next user starts as soon as a connection is free.
const MAX_VUS = Number(__ENV.MAX_VUS || 1000);
const VUS = Math.min(USERS, MAX_VUS);
const OTP = __ENV.OTP; // the stack's OTP_DEV_CODE
if (!OTP) throw new Error("set -e OTP=<OTP_DEV_CODE from deploy/.env>");

const allUsers = new SharedArray("users", () => JSON.parse(open("./users.json")));

const winners = new Counter("winners");           // users who locked a seat
const losers = new Counter("losers");             // users who ended with nothing
const booked = new Counter("booked");             // winners whose hold + OTP succeeded
const clicks = new Counter("clicks");
const taken = new Counter("seat_taken");
const otherAnswers = new Counter("other_answers");
const clickMs = new Trend("click_ms", true);
const seenOther = {};

http.setResponseCallback(http.expectedStatuses(200, 201, 202, 204, 403, 404, 409, 422, 429));

export const options = {
  setupTimeout: "120s",
  teardownTimeout: "120s",
  scenarios: {
    rush: USERS <= MAX_VUS
      ? { executor: "per-vu-iterations", vus: USERS, iterations: 1, maxDuration: "2m" }
      : { executor: "shared-iterations", vus: VUS, iterations: USERS, maxDuration: "30m" },
  },
  thresholds: {
    winners: [`count<=${SEATS}`],          // never more winners than seats
    http_req_failed: ["rate<0.01"],
    click_ms: ["p(99)<1000"],
  },
  summaryTrendStats: ["avg", "med", "p(95)", "p(99)", "max"],
};

const auth = (u, extra) => ({ headers: Object.assign({ Authorization: `Bearer ${u.token}` }, extra || {}) });

// Seats of `cls` on the seat map that are free for the trip from..to.
function freeSeats(sm, cls, from, to, seats) {
  const occ = new Uint8Array(encoding.b64decode(sm.seats, "std"));
  const pool = new Uint8Array(encoding.b64decode(sm.seat_pool, "std"));
  const pi = sm.pools.findIndex((p) => p.class_code === cls && p.quota === "ONLINE");
  return seats.filter((s) => {
    if (pool[s.idx] !== pi) return false;
    for (let k = from; k < to; k++) if ((occ[s.idx * 8 + (k >> 3)] >> (k & 7)) & 1) return false;
    return true;
  });
}

export function setup() {
  if (JOURNEYS.length === 0) throw new Error("set JOURNEY=<journey_id> or JOURNEYS=<id>,<id>,...");
  if (USERS > allUsers.length) throw new Error(`USERS=${USERS} but users.json has ${allUsers.length}`);
  const u = allUsers[0];
  let free = [];
  const lines = [];
  for (const jid of JOURNEYS) {
    const j = http.get(`${BASE}/v1/journeys/${jid}`, auth(u)).json();
    const pools = j.pools.filter((p) => p.quota === "ONLINE");
    const classes = CLASSES === "all" ? pools.map((p) => p.class_code)
      : CLASSES === "largest" ? [pools.slice().sort((a, b) => b.seats - a.seats)[0].class_code]
        : [CLASSES];
    const from = JOURNEYS.length === 1 ? Number(__ENV.FROM || 0) : 0;
    const to = JOURNEYS.length === 1 ? Number(__ENV.TO || j.route.length - 1) : j.route.length - 1;
    const layout = http.get(`${BASE}/v1/layouts/${j.layout_id}`, auth(u)).json();
    const smRes = http.get(`${BASE}/v1/journeys/${jid}/seatmap`, auth(u));
    if (smRes.status !== 200) throw new Error(`seat map of ${jid}: ${smRes.status}; is the train on sale?`);
    const sm = smRes.json();
    for (const cls of classes) {
      const seats = [];
      for (const c of layout.coaches) {
        if (c.class_code !== cls) continue;
        for (const s of c.seats) seats.push({ j: jid, cls, from, to, idx: s.seat_idx, id: s.seat_id });
      }
      const f = freeSeats(sm, cls, from, to, seats);
      lines.push(`${j.train_name} (${jid}) ${cls}: ${f.length} free`);
      free = free.concat(f);
    }
  }
  if (free.length < SEATS) throw new Error(`only ${free.length} free seats: ${lines.join("; ")}`);
  // SEATS random free seats are the only "tickets" on offer
  const target = [];
  while (target.length < SEATS) target.push(free.splice(Math.floor(Math.random() * free.length), 1)[0]);
  const start = Date.now() + 5000 + VUS * 5; // give every VU time to be ready, then all go at once
  console.log(`${USERS} users -> ${SEATS} seats, picked from: ${lines.join("; ")}`);
  return { target, start };
}

export default function (d) {
  const u = allUsers[exec.scenario.iterationInTest]; // one iteration = one person = one account
  const wait = (d.start - Date.now()) / 1000;
  if (wait > 0) sleep(wait);

  const tried = new Set();
  for (let attempt = 0; attempt < TRIES; attempt++) {
    // a seat this user has not tried yet (people do not click the same taken seat again)
    let t;
    for (let guard = 0; guard < 20; guard++) {
      const c = d.target[Math.floor(Math.random() * d.target.length)];
      if (!tried.has(c.j + "/" + c.id)) { t = c; break; }
    }
    if (!t) break;
    tried.add(t.j + "/" + t.id);

    const res = http.put(`${BASE}/v1/journeys/${t.j}/selection/${t.id}`,
      JSON.stringify({ from_idx: t.from, to_idx: t.to, class_code: t.cls }),
      Object.assign(auth(u, { "Content-Type": "application/json", "X-Admission-Token": u.admission }),
        { tags: { name: "select" } }));
    clicks.add(1);
    clickMs.add(res.timings.duration);

    if (res.status === 200) {
      winners.add(1);
      book(u, t);
      return;
    }
    const code = errorCode(res);
    if (code === "SEAT_TAKEN") { taken.add(1); continue; }
    otherAnswers.add(1, { code });
    if (!seenOther[code]) {
      seenOther[code] = true;
      console.warn(`unexpected click answer: status ${res.status} ${code} ${res.error || ""} ${String(res.body || "").slice(0, 200)}`);
    }
    break; // anything else (rate limit, rule, error): give up like a real user would
  }
  losers.add(1);
}

function book(u, t) {
  const h = http.post(`${BASE}/v1/holds`,
    JSON.stringify({ journey_id: Number(t.j), passengers: [{ name: `Load User ${u.user_id}`, gender: "MALE", type: "ADULT" }] }),
    Object.assign(auth(u, { "Content-Type": "application/json", "X-Admission-Token": u.admission,
      "Idempotency-Key": uuid() }), { tags: { name: "hold" } }));
  if (h.status !== 201) {
    console.warn(`winner ${u.user_id} (${t.j} ${t.id}) could not hold: ${h.status} ${h.body}`);
    return;
  }
  const v = http.post(`${BASE}/v1/holds/${h.json("pnr")}/otp/verify`, JSON.stringify({ otp: OTP }),
    Object.assign(auth(u, { "Content-Type": "application/json" }), { tags: { name: "otp" } }));
  if (v.status === 200) {
    booked.add(1);
    console.log(`WINNER user ${u.user_id} got ${t.j} ${t.id}, PNR ${h.json("pnr")}`);
  }
}

export function teardown(d) {
  sleep(2); // let the last seat-map snapshots (published at most once a second) catch up
  const u = allUsers[0];
  let stillFree = [];
  for (const jid of JOURNEYS) {
    const mine = d.target.filter((t) => t.j === jid);
    if (mine.length === 0) continue;
    const sm = http.get(`${BASE}/v1/journeys/${jid}/seatmap`, auth(u)).json();
    for (const cls of [...new Set(mine.map((t) => t.cls))]) {
      const ofCls = mine.filter((t) => t.cls === cls);
      stillFree = stillFree.concat(freeSeats(sm, cls, ofCls[0].from, ofCls[0].to, ofCls).map((t) => `${jid} ${t.id}`));
    }
    const inv = http.get(`${INTERNAL}/internal/v1/journeys/${jid}/invariants`,
      { headers: { "X-Internal-Token": INTERNAL_TOKEN } });
    console.log(`CHECK invariants ${jid}: ${inv.body}`);
  }
  console.log(stillFree.length === 0
    ? `CHECK all ${d.target.length} target seats are taken`
    : `CHECK ${stillFree.length} target seats still free (fewer winners than seats): ${stillFree.slice(0, 20).join(", ")}`);
}

// The API's error code, or a description when the answer is not the API's JSON (timeouts, proxy errors).
function errorCode(res) {
  if (res.status === 0) return `NETWORK_${res.error_code || "ERROR"}`;
  if (res.status < 400) return String(res.status);
  try {
    const c = res.json("error.code");
    if (c) return String(c);
  } catch (_) { /* not JSON */ }
  return `HTTP_${res.status}`;
}

function uuid() {
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    return (c === "x" ? r : (r & 0x3) | 0x8).toString(16);
  });
}
