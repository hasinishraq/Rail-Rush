import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom";
import {
  admissionExpiry, admissionToken, api, apiWithStatus, ApiError, dropAdmission, idemKey, settle, storeAdmission, streamSeatMap,
  type AcceptedResponse, type AdmissionResponse, type CartResponse, type HoldResponse, type JourneyResponse,
  type LayoutResponse, type PassengerIn, type Page, type SavedPassenger, type SeatMapResponse, type SelectRequest, type StopRef,
} from "../api";
import CoachMap, { RouteKey, seatState, type SeatState } from "../components/CoachMap";
import Steps from "../components/Steps";
import { classHint, className, countdown, dateTime, MAX_SEATS, time } from "../format";
import { num, t } from "../i18n";
import { stationName } from "../stations";
import { useMedia, useNow, usePoll } from "../hooks";
import { applyDelta, decodeSeatMap, mask, type DecodedMap } from "../seatmap";

const QUOTA = "ONLINE";
const emptyPax = (): PassengerIn => ({ name: "", gender: "", type: "ADULT" });

type Feed = "live" | "polling" | "connecting";

export default function Journey() {
  const { id } = useParams();
  const journeyId = Number(id);
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const now = useNow();
  // Wide screens show the coach lying along the page; phones show it standing up.
  const wide = useMedia("(min-width: 1000px)");

  const [journey, setJourney] = useState<JourneyResponse | null>(null);
  const [layout, setLayout] = useState<LayoutResponse | null>(null);
  const [saved, setSaved] = useState<SavedPassenger[]>([]);
  const [loadErr, setLoadErr] = useState("");

  const from = Number(params.get("from") ?? 0);
  const to = Number(params.get("to") ?? (journey ? journey.route.length - 1 : 1));
  const cls = params.get("class") ?? "S_CHAIR";

  // Waiting room: only admitted users may lock seats and stream deltas.
  const [admission, setAdmission] = useState<string | null>(() => admissionToken(journeyId));
  const [position, setPosition] = useState<number | null>(null);
  const [queueErr, setQueueErr] = useState("");

  // Seat map: a CDN snapshot for everyone, live deltas for admitted users.
  const [map, setMap] = useState<DecodedMap | null>(null);
  const mapRef = useRef<DecodedMap | null>(null);
  const [meta, setMeta] = useState<Pick<SeatMapResponse, "pools" | "segfree"> | null>(null);
  const [notOpen, setNotOpen] = useState<string | null>(null);
  const [feed, setFeed] = useState<Feed>("polling");
  const [coachName, setCoachName] = useState("");

  // Cart (my locked seats), passengers, hold.
  const [cart, setCart] = useState<CartResponse | null>(null);
  const [pending, setPending] = useState<Set<string>>(new Set());
  const [pax, setPax] = useState<PassengerIn[]>([]);
  const [fieldErr, setFieldErr] = useState<Record<string, string>>({});
  const [notice, setNotice] = useState<{ text: string; pnr?: string; release?: boolean } | null>(null);
  const [busy, setBusy] = useState(false);
  const holdKey = useRef(idemKey());

  // Phones: the bottom "seats chosen" bar hides while the passenger form is on screen.
  const [paxEl, setPaxEl] = useState<HTMLElement | null>(null);
  const [paxInView, setPaxInView] = useState(false);
  useEffect(() => {
    if (!paxEl || typeof IntersectionObserver === "undefined") return;
    const io = new IntersectionObserver(([e]) => setPaxInView(e.isIntersecting), { threshold: 0.15 });
    io.observe(paxEl);
    return () => io.disconnect();
  }, [paxEl]);

  const setMapBoth = (m: DecodedMap | null) => {
    mapRef.current = m;
    setMap(m);
  };

  useEffect(() => {
    (async () => {
      try {
        const j = await api<JourneyResponse>("GET", `/v1/journeys/${journeyId}`);
        setJourney(j);
        setLayout(await api<LayoutResponse>("GET", `/v1/layouts/${j.layout_id}`));
      } catch (e) {
        setLoadErr((e as ApiError).status === 404 ? t("j.notFound") : (e as Error).message);
      }
    })();
    api<Page<SavedPassenger>>("GET", "/v1/me/passengers").then((p) => setSaved(p.items)).catch(() => undefined);
    // Restore the cart after a refresh or app restart (§13.3).
    api<CartResponse>("GET", `/v1/journeys/${journeyId}/selection`).then(adoptCart).catch(() => undefined);
  }, [journeyId]);

  const adoptCart = useCallback((c: CartResponse | null) => {
    const live = c && c.seats.length > 0 ? c : null;
    setCart(live);
    if (live) {
      setParams((p) => {
        const n = new URLSearchParams(p);
        n.set("from", String(live.from_idx));
        n.set("to", String(live.to_idx));
        n.set("class", live.class_code);
        return n;
      }, { replace: true });
    }
  }, [setParams]);

  // Keep one passenger row per locked seat, in cart order.
  useEffect(() => {
    const n = cart?.seats.length ?? 0;
    setPax((p) => (p.length === n ? p : Array.from({ length: n }, (_, i) => p[i] ?? emptyPax())));
  }, [cart?.seats.length]);

  // The cart timer: locks are released at expires_at if passenger details were not sent.
  const cartLeft = cart?.expires_at ? new Date(cart.expires_at).getTime() - now : 0;
  useEffect(() => {
    if (cart && cart.expires_at && cartLeft <= 0) {
      setCart(null);
      setNotice({ text: t("j.released") });
    }
  }, [cart, cartLeft]);

  async function snapshot(): Promise<number | null> {
    try {
      const sm = await api<SeatMapResponse>("GET", `/v1/journeys/${journeyId}/seatmap`);
      setNotOpen(null);
      setMeta({ pools: sm.pools, segfree: sm.segfree });
      const cur = mapRef.current;
      if (!cur || sm.version >= cur.version) setMapBoth(decodeSeatMap(sm));
      return sm.version;
    } catch (e) {
      const a = e as ApiError;
      if (a.code === "NOT_OPEN") setNotOpen(String(a.details.opens_at ?? ""));
      return null;
    }
  }

  // While the stream is not live (connecting, blocked, overloaded): poll the CDN snapshot every 2 s.
  usePoll(async () => { await snapshot(); }, 2000, feed !== "live");

  // Everyone watching, admitted or still in the queue: snapshot once, then the
  // live delta stream; resync on a gap. Admission is only needed to lock seats.
  useEffect(() => {
    const ctl = new AbortController();
    let stopped = false;
    (async () => {
      let failures = 0;
      while (!stopped) {
        setFeed("connecting");
        const v = await snapshot();
        if (v === null) {
          setFeed("polling");
          await new Promise((r) => setTimeout(r, 3000));
          continue;
        }
        const inner = new AbortController();
        ctl.signal.addEventListener("abort", () => inner.abort());
        let replaced = false;
        try {
          setFeed("live");
          await streamSeatMap(journeyId, v, (ev) => {
            if (ev.type === "delta") {
              const cur = mapRef.current;
              const next = cur && applyDelta(cur, ev.delta);
              if (next) setMapBoth(next);
              else inner.abort(); // gap: refetch the snapshot
            } else {
              replaced = ev.type === "reconnect" && ev.reason === "replaced";
              inner.abort(); // resync or reconnect
            }
          }, inner.signal);
          failures = 0;
        } catch (e) {
          if (stopped) return;
          if (!(e instanceof DOMException)) failures++;
        }
        if (replaced) {
          // One stream per user and train: another tab took it over. Poll here
          // instead of taking it back, or the two tabs would keep swapping it.
          setFeed("polling");
          await new Promise((r) => setTimeout(r, 60_000));
          continue;
        }
        if (failures > 0) {
          setFeed("polling"); // SSE blocked or overloaded: fall back to polling for a while
          await new Promise((r) => setTimeout(r, Math.min(30_000, 2000 * failures)));
        }
      }
    })();
    return () => {
      stopped = true;
      ctl.abort();
    };
  }, [journeyId]);

  const poolIndex = meta?.pools.findIndex((p) => p.class_code === cls && p.quota === QUOTA) ?? -1;
  const poolOpen = poolIndex >= 0 && meta!.pools[poolIndex].open;
  const pool = journey?.pools.find((p) => p.class_code === cls && p.quota === QUOTA);
  const tripMask = useMemo(() => mask(from, to), [from, to]);
  const coaches = useMemo(() => layout?.coaches.filter((c) => c.class_code === cls) ?? [], [layout, cls]);
  const coach = coaches.find((c) => c.coach === coachName) ?? coaches[0];
  const stopCodes = journey?.route.map((s) => s.station_code) ?? [];
  const mine = useMemo(() => new Set(cart?.seats.map((s) => s.seat_id) ?? []), [cart]);
  const segfree = meta?.segfree[`${cls}/${QUOTA}`];
  const soldOut = !!segfree && Math.min(...segfree.slice(from, to)) === 0;
  const maxSeats = pool?.max_seats_per_user ?? MAX_SEATS;

  // ---- waiting room (POST /v1/admission locally; the vendor's queue in production).
  // Nobody presses a button: the page joins by itself once the seat map shows the
  // train is on sale, and joins again whenever the admission is lost.
  const mapReady = meta !== null;
  const onSale = journey?.status === "SCHEDULED";
  useEffect(() => {
    if (admission || !mapReady || notOpen || !onSale) return;
    let stopped = false;
    (async () => {
      while (!stopped) {
        try {
          const r = await api<AdmissionResponse>("POST", "/v1/admission", { journey_id: journeyId });
          if (stopped) return;
          setQueueErr("");
          if (r.admitted && r.admission_token && r.expires_at) {
            storeAdmission(journeyId, r.admission_token, r.expires_at);
            setPosition(null);
            setAdmission(r.admission_token);
            return;
          }
          setPosition(r.position ?? null);
          await new Promise((res) => setTimeout(res, 1500));
        } catch (e) {
          if (stopped) return;
          setQueueErr((e as Error).message);
          await new Promise((res) => setTimeout(res, 5000));
        }
      }
    })();
    return () => {
      stopped = true;
    };
  }, [admission, mapReady, notOpen, onSale, journeyId]);

  // Rejoin shortly before the admission runs out, so a long session never loses it mid-click.
  useEffect(() => {
    if (!admission) return;
    const t = setTimeout(() => {
      dropAdmission(journeyId);
      setAdmission(null);
    }, Math.max(0, admissionExpiry(journeyId) - Date.now() - 30_000));
    return () => clearTimeout(t);
  }, [admission, journeyId]);

  // A lost admission is fetched again by the waiting-room effect. A used one is
  // replaced too, but the caller still tells the user about the booking it made.
  function lostAdmission(a: ApiError) {
    if (!a.code?.startsWith("ADMISSION")) return false;
    dropAdmission(journeyId);
    setAdmission(null);
    return a.code !== "ADMISSION_USED";
  }

  // ---- select / unselect: every click is one request (§13)
  async function clickSeat(seatId: string, state: SeatState) {
    if (!admission) {
      setNotice({ text: t("j.letIn") });
      return;
    }
    if (state !== "free" && state !== "mine") return;
    if (state === "free" && cart && cart.seats.length >= Math.min(maxSeats, cart.max_seats ?? maxSeats)) {
      setNotice({ text: t("j.maxSeats", { max: maxSeats }) });
      return;
    }
    setPending((p) => new Set(p).add(seatId));
    setNotice(null);
    try {
      const url = `/v1/journeys/${journeyId}/selection/${seatId}`;
      const body: SelectRequest = { from_idx: from, to_idx: to, class_code: cls, quota: QUOTA };
      const r = await settle(await apiWithStatus<CartResponse | AcceptedResponse>(
        state === "free" ? "PUT" : "DELETE", url, state === "free" ? body : undefined, { headers: { "X-Admission-Token": admission } }));
      adoptCart(r.data);
    } catch (e) {
      const a = e as ApiError;
      if (!lostAdmission(a)) setNotice(explain(a, seatId));
    } finally {
      setPending((p) => {
        const n = new Set(p);
        n.delete(seatId);
        return n;
      });
    }
  }

  async function releaseAll() {
    if (!admission) return;
    setBusy(true);
    try {
      const r = await settle(await apiWithStatus<CartResponse | AcceptedResponse>("DELETE", `/v1/journeys/${journeyId}/selection/*`,
        undefined, { headers: { "X-Admission-Token": admission } }));
      adoptCart(r.data);
      setNotice(null);
    } catch (e) {
      const a = e as ApiError;
      if (!lostAdmission(a)) setNotice(explain(a));
    } finally {
      setBusy(false);
    }
  }

  const setTrip = (k: "from" | "to", v: number) => {
    const n = new URLSearchParams(params);
    n.set(k, String(v));
    setParams(n, { replace: true });
  };
  const setClass = (c: string) => {
    const n = new URLSearchParams(params);
    n.set("class", c);
    setParams(n, { replace: true });
    setCoachName("");
  };

  const updatePax = (i: number, patch: Partial<PassengerIn>) => setPax((p) => p.map((x, k) => (k === i ? { ...x, ...patch } : x)));
  const useSaved = (s: SavedPassenger) => {
    const i = pax.findIndex((p) => !p.name.trim());
    if (i >= 0) updatePax(i, { name: s.name, gender: s.gender, type: s.type || "ADULT" });
  };

  function validate(): boolean {
    const errs: Record<string, string> = {};
    pax.forEach((p, i) => {
      if (!p.name.trim()) errs[`${i}.name`] = "Enter the passenger's name";
      if (!p.gender) errs[`${i}.gender`] = "Choose one";
    });
    setFieldErr(errs);
    return Object.keys(errs).length === 0;
  }

  // ---- passenger details -> 10-minute hold + OTP (§14.1, W7)
  async function submitPassengers() {
    if (!validate() || !admission || !cart) return;
    setBusy(true);
    setNotice(null);
    try {
      const r = await settle(await apiWithStatus<HoldResponse | AcceptedResponse>("POST", "/v1/holds",
        { journey_id: journeyId, passengers: pax.map((p) => ({ ...p, name: p.name.trim() })) },
        { headers: { "Idempotency-Key": holdKey.current, "X-Admission-Token": admission } }));
      const h = r.data;
      navigate(`/bookings/${h.pnr}`, { state: { hold: h } });
    } catch (e) {
      const a = e as ApiError;
      if (a.status && a.status < 500 && a.code !== "RATE_LIMITED" && a.code !== "OVERLOADED") holdKey.current = idemKey();
      if (a.code === "EMPTY_CART") setCart(null);
      if (!lostAdmission(a)) setNotice(explain(a));
    } finally {
      setBusy(false);
    }
  }

  if (loadErr) return <p className="empty">{loadErr} <Link to="/">{t("j.searchAgain")}</Link></p>;
  if (!journey || !layout) return <p className="loading">{t("j.loading")}</p>;
  const fromStop = journey.route[from], toStop = journey.route[to];
  const stop = (s?: StopRef) => (s ? stationName(s.station_code, s.station_name) : "");
  const freeInCoach = (c: typeof coaches[number]) =>
    c.seats.filter((s) => seatState(map, s.seat_idx, poolIndex, tripMask, false, false) === "free").length;
  const salesOpen = journey.status === "SCHEDULED";

  return (
    <div className="journey">
      <Steps current={cart ? 2 : 1} />
      <section className="journey-head">
        <div className="jh-train">
          <h1 className="train-name">{journey.train_name.replace(/\s*\(\d+\)$/, "")}</h1>
          <span className="train-no">{t("train.no", { n: num(journey.train_no) })}</span>
        </div>
        <div className="jh-trip">
          <label className="field compact">
            <span>{t("j.boarding")}</span>
            <select value={from} disabled={!!cart} onChange={(e) => setTrip("from", Number(e.target.value))}>
              {journey.route.slice(0, -1).map((s) => (
                <option key={s.idx} value={s.idx} disabled={s.idx >= to}>{stop(s)}, {time(s.time)}</option>
              ))}
            </select>
          </label>
          <label className="field compact">
            <span>{t("j.alighting")}</span>
            <select value={to} disabled={!!cart} onChange={(e) => setTrip("to", Number(e.target.value))}>
              {journey.route.slice(1).map((s) => (
                <option key={s.idx} value={s.idx} disabled={s.idx <= from}>{stop(s)}, {time(s.time)}</option>
              ))}
            </select>
          </label>
          <label className="field compact">
            <span>{t("j.class")}</span>
            <select value={cls} disabled={!!cart} onChange={(e) => setClass(e.target.value)}>
              {journey.pools.filter((p) => p.quota === QUOTA).map((p) => (
                <option key={p.class_code} value={p.class_code}>{className(p.class_code)}{classHint(p.class_code) ? ` (${classHint(p.class_code)})` : ""}</option>
              ))}
            </select>
          </label>
        </div>
        <p className="jh-when">
          {t("j.when", {
            from: stop(fromStop), fromTime: dateTime(fromStop?.time ?? journey.run_date),
            to: stop(toStop), toTime: dateTime(toStop?.time ?? journey.run_date),
          })}
        </p>
        {cart && (
          <p className="jh-locked">
            {t("j.lockedA")} <button className="linkish" onClick={releaseAll} disabled={busy}>{t("j.lockedB")}</button>
          </p>
        )}
      </section>

      {!salesOpen && <p className="notice">{t("j.closed")}</p>}

      <div className="book-grid">
        <section className="seat-section" aria-label={t("j.seatsAria")}>
          {notOpen ? (
            <div className="queue">
              <h2>{t("j.notOpenTitle")}</h2>
              <p>{t("j.notOpenText", { when: dateTime(notOpen) })}</p>
              <p className="queue-pos">{countdown(new Date(notOpen).getTime() - now)}</p>
            </div>
          ) : (
            <>
              <div className="map-bar">
                <h2 className="map-title">{t("j.mapTitle")}</h2>
                <span className={`feed feed-${feed}`}>
                  {feed === "live" ? t("j.feedLive") : feed === "connecting" ? t("j.feedConnecting") : t("j.feedPolling")}
                </span>
              </div>
              {!poolOpen && meta && <p className="notice">{t("j.classNotOnSale")}</p>}
              <div className="coach-tabs" role="tablist" aria-label={t("j.coaches")}>
                {coaches.map((c) => (
                  <button key={c.coach} role="tab" aria-selected={c === coach} className={c === coach ? "active" : ""} onClick={() => setCoachName(c.coach)}>
                    <span className="ct-name">{t("j.coach", { c: c.coach })}</span>
                    <span className="ct-free">{map ? t("j.free", { n: freeInCoach(c) }) : "…"}</span>
                  </button>
                ))}
              </div>
              <div className="legend" aria-label={t("legend.label")}>
                <span><i className="lg lg-free" /> {t("legend.free")}</span>
                <span><i className="lg lg-selected" /> {t("legend.mine")}</span>
                <span><i className="lg lg-taken" /> {t("legend.taken")}</span>
                <span><i className="lg lg-other" /> {t("legend.other")}</span>
                <span className="lg-types">{t("legend.types")}</span>
              </div>
              {coach && (
                <CoachMap coach={coach} map={map} poolIndex={poolIndex} from={from} to={to} tripMask={tripMask}
                  mine={mine} pending={pending} onClick={clickSeat} stopCodes={stopCodes} vertical={!wide} />
              )}
              <details className="strip-help">
                <summary>{t("j.barsQ")}</summary>
                <p>{t("j.barsA")}</p>
                <RouteKey stopCodes={stopCodes} from={from} to={to} />
              </details>
              {soldOut && <p className="muted">{t("j.soldOut")}</p>}
            </>
          )}
        </section>

        {cart && !paxInView && (
          <div className="cart-dock">
            <span>{cart.seats.length === 1 ? t("j.dock1", { left: countdown(cartLeft) }) : t("j.dockN", { n: cart.seats.length, left: countdown(cartLeft) })}</span>
            <a className="btn btn-primary" href="#pax">{t("j.continue")}</a>
          </div>
        )}
        <aside className="pax-panel" id="pax" ref={setPaxEl} aria-label={t("j.panelAria")}>
          {!admission ? (
            <div className="queue-box">
              <h2>{t("j.waitTitle")}</h2>
              {!salesOpen ? (
                <p>{t("j.closed")}</p>
              ) : notOpen ? (
                <p>{t("j.waitNotOpen")}</p>
              ) : (
                <>
                  <p>{t("j.waitQueue")}</p>
                  <p className="queue-pos" aria-live="polite">{position ? t("j.ahead", { n: position }) : t("j.gettingIn")}</p>
                </>
              )}
              {queueErr && <p className="fineprint">{t("j.retrying", { err: queueErr })}</p>}
              {pool && new Date(pool.opens_at).getTime() > now && (
                <p className="fineprint">{t("j.salesOpenIn", { when: dateTime(pool.opens_at), left: countdown(new Date(pool.opens_at).getTime() - now) })}</p>
              )}
            </div>
          ) : !cart ? (
            <div className="queue-box">
              <h2>{t("j.tapTitle")}</h2>
              <p>{t("j.tapText", { max: maxSeats })}</p>
            </div>
          ) : (
            <>
              <div className="cart-head">
                <h2>{t("j.yourSeats")}</h2>
                <div className="cart-timer" role="timer" aria-label={t("j.keptFor", { left: countdown(cartLeft) })}>
                  <span className="sb-clock">{countdown(cartLeft)}</span>
                  <span>{t("j.minutesLeft")}</span>
                </div>
              </div>
              <ul className="cart-seats">
                {cart.seats.map((s) => (
                  <li key={s.seat_id}>
                    <span className="pax-seat">{s.seat_id}</span>
                    <button className="linkish" disabled={pending.has(s.seat_id)} onClick={() => clickSeat(s.seat_id, "mine")}
                      aria-label={t("j.giveBack", { seat: s.seat_id })}>{t("j.remove")}</button>
                  </li>
                ))}
              </ul>
              <p className="pax-intro">{t("j.who")}</p>
              {saved.length > 0 && (
                <div className="saved-chips" aria-label={t("j.savedAria")}>
                  {saved.map((s) => (
                    <button key={s.id} type="button" className="chip" onClick={() => useSaved(s)} disabled={pax.every((p) => p.name.trim())}>+ {s.name}</button>
                  ))}
                </div>
              )}
              <ol className="pax-list">
                {pax.map((p, i) => (
                  <li key={i} className="pax">
                    <div className="pax-top"><span>{t("j.pax", { n: i + 1 })}</span><span className="pax-seat">{t("j.seat", { seat: cart.seats[i]?.seat_id ?? "" })}</span></div>
                    <label className="field">
                      <span>{t("f.fullName")}</span>
                      <input value={p.name} maxLength={40} onChange={(e) => updatePax(i, { name: e.target.value })} />
                      {fieldErr[`${i}.name`] && <span className="field-error">{t("j.errName")}</span>}
                    </label>
                    <div className="pax-row">
                      <label className="field">
                        <span>{t("f.gender")}</span>
                        <select value={p.gender} onChange={(e) => updatePax(i, { gender: e.target.value })}>
                          <option value="">{t("f.choose")}</option>
                          <option value="FEMALE">{t("f.female")}</option>
                          <option value="MALE">{t("f.male")}</option>
                          <option value="OTHER">{t("f.other")}</option>
                        </select>
                        {fieldErr[`${i}.gender`] && <span className="field-error">{t("j.errGender")}</span>}
                      </label>
                      <label className="field">
                        <span>{t("f.ticket")}</span>
                        <select value={p.type ?? "ADULT"} onChange={(e) => updatePax(i, { type: e.target.value })}>
                          <option value="ADULT">{t("f.adult")}</option>
                          <option value="CHILD">{t("f.child")}</option>
                        </select>
                      </label>
                    </div>
                  </li>
                ))}
              </ol>
              <button className="btn btn-primary btn-wide" disabled={busy || pending.size > 0} onClick={submitPassengers}>
                {busy ? t("j.holding") : cart.seats.length === 1 ? t("j.continue1") : t("j.continueN", { n: cart.seats.length })}
              </button>
              <p className="fineprint">{t("j.nextFine")}</p>
            </>
          )}
          {notice && (
            <p className="error" role="alert">
              {notice.text} {notice.pnr && <Link to={`/bookings/${notice.pnr}`}>{t("j.openBooking", { pnr: notice.pnr })}</Link>}
              {notice.release && <> <button className="linkish" onClick={releaseAll}>{t("j.giveBackAll")}</button></>}
            </p>
          )}
        </aside>
      </div>
    </div>
  );
}

function explain(a: ApiError, seat?: string): { text: string; pnr?: string; release?: boolean } {
  const d = a.details ?? {};
  switch (a.code) {
    case "SEAT_TAKEN":
      return { text: t("e.SEAT_TAKEN", { seat: seat ?? t("j.thatSeat") }) };
    case "SELECTION_LIMIT":
      return { text: t("e.SELECTION_LIMIT", { max: Number(d.max ?? MAX_SEATS) }) };
    case "ORDER_LIMIT_EXCEEDED":
      return { text: t("e.ORDER_LIMIT_EXCEEDED", { issued: Number(d.issued), limit: Number(d.limit) }) };
    case "CART_CONFLICT":
      return { text: t("e.CART_CONFLICT"), release: true };
    case "ACTIVE_HOLD_EXISTS":
      return { text: t("e.ACTIVE_HOLD_EXISTS"), pnr: d.pnr as string };
    case "EMPTY_CART":
      return { text: t("e.EMPTY_CART") };
    case "PASSENGER_COUNT_MISMATCH":
      return { text: t("e.PASSENGER_COUNT_MISMATCH") };
    case "SOLD_OUT":
      return { text: t("e.SOLD_OUT") };
    case "NOT_OPEN":
      return { text: t("e.NOT_OPEN", { when: d.opens_at ? dateTime(String(d.opens_at)) : t("e.later") }) };
    case "SALES_CLOSED":
      return { text: t("j.closed") };
    case "SEAT_NOT_IN_POOL":
      return { text: t("e.SEAT_NOT_IN_POOL") };
    case "ADMISSION_USED":
      return { text: t("e.ADMISSION_USED"), pnr: d.pnr as string };
    case "ADMISSION_REQUIRED":
    case "ADMISSION_INVALID":
      return { text: t("e.ADMISSION_INVALID") };
    case "RATE_LIMITED":
    case "OVERLOADED":
      return { text: t("e.RATE_LIMITED", { s: d.retry_after !== undefined ? Number(d.retry_after) : t("e.few") }) };
    case "LOG_UNAVAILABLE":
      return { text: t("e.LOG_UNAVAILABLE") };
    case "VALIDATION_FAILED":
      return { text: Object.values((d.fields as Record<string, string>) ?? {}).join(". ") || a.message };
  }
  return { text: a.message };
}
