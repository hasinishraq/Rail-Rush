// Typed client for the booking API (spec v4.0, Part II). Access tokens are
// refreshed silently when fewer than 2 minutes remain, and once more on
// TOKEN_EXPIRED. Times are UTC (RFC 3339); money is poisha + "BDT".

export interface Money { amount_minor: number; currency: string }
export interface StopRef { idx: number; station_code: string; station_name: string; time: string }
export interface UserInfo { user_id: number; name: string; phone: string; verified: boolean }
export interface TokenResponse {
  access_token: string; token_type: string; expires_in: number;
  refresh_token: string; refresh_expires_in: number; user: UserInfo;
}
export interface Me extends UserInfo { email?: string; role: string }
export interface PassengerIn { name: string; gender: string; type?: string }
export interface SavedPassenger extends PassengerIn { id: number }
export interface SeatRef { seat_id: string; seat_idx: number; coach: string; seat_no: number }
export interface PassengerSeat {
  seq: number; name: string; gender: string; type: string;
  status: string; seat: SeatRef; fare: Money;
}
export interface Station { code: string; name: string; name_bn?: string; city: string }
export interface ClassAvail { class_code: string; fare: Money; status: string; count: number; opens_at?: string }
export interface TrainOption {
  journey_id: number; train_no: string; train_name: string; from: StopRef; to: StopRef;
  duration_min: number; classes: ClassAvail[];
}
export interface SearchResponse { from: string; to: string; date: string; generated_at: string; trains: TrainOption[] }
export interface PoolInfo { class_code: string; quota: string; opens_at: string; seats: number; max_seats_per_user: number }
export interface JourneyResponse {
  journey_id: number; train_no: string; train_name: string; run_date: string; status: string;
  layout_id: number; sales_close_at: string; route: StopRef[]; pools: PoolInfo[];
}
export interface Seat { seat_idx: number; seat_id: string; seat_no: number; seat_type: string; x: number; y: number }
export interface Coach { coach: string; class_code: string; width: number; height: number; seats: Seat[] }
export interface LayoutResponse { layout_id: number; coaches: Coach[] }
export interface SeatMapPool { class_code: string; quota: string; open: boolean; max_seats_per_user: number }
export interface SeatMapResponse {
  journey_id: number; version: number; generated_at: string; segments: number; encoding: string;
  seats: string; seat_pool: string; pools: SeatMapPool[]; segfree: Record<string, number[]>; live_url: string;
}
export interface SeatDelta { from_v: number; v: number; changes: string; at?: string }
export interface AdmissionResponse { admitted: boolean; admission_token?: string; expires_at?: string; position?: number }
export interface RegisterResponse { user_id: number }
export interface VerifyOTPResponse { pnr: string; status: string; expires_at: string }
export interface CreatePaymentRequest { pnr: string; method: string; return_url: string }
export interface CancelRequest { passenger_seqs?: number[]; reason?: string }
export interface SelectRequest { from_idx: number; to_idx: number; class_code: string; quota?: string }
export interface CartResponse {
  journey_id: number; from_idx: number; to_idx: number; class_code: string; seats: SeatRef[];
  max_seats: number; expires_at?: string | null; seatmap_version: number;
}
export interface OTPInfo { sent_to: string; expires_at: string; resend_after: string }
export interface CreateHoldRequest { journey_id: number; passengers: PassengerIn[] }
export interface HoldResponse {
  request_id: string; pnr: string; status: string; journey_id: number; train_name: string; from: StopRef; to: StopRef;
  class_code: string; passengers: PassengerSeat[]; fare: Money; service_charge: Money; amount: Money;
  expires_at: string; otp: OTPInfo;
}
export interface AcceptedResponse { request_id: string; poll_url: string; stream_url: string }
export interface RequestStatus { request_id: string; state: "PENDING" | "DONE"; status_code?: number; body?: unknown }
export interface PaymentResponse {
  payment_id: string; pnr: string; status: string; method: string; amount: Money;
  redirect_url?: string; hold_expires_at: string; captured_at?: string; booking_status?: string;
}
export interface RefundBrief { refund_id: string; reason: string; amount: Money; status: string }
export interface BookingResponse {
  pnr: string; status: string; journey_id: number; train_no: string; train_name: string; run_date: string;
  from: StopRef; to: StopRef; class_code: string; passengers: PassengerSeat[]; amount: Money;
  hold_expires_at?: string; payment?: { payment_id: string; status: string; method: string; amount: Money };
  refunds?: RefundBrief[]; seats_changed?: boolean; updated_at: string;
}
export interface BookingSummary {
  pnr: string; status: string; train_no: string; run_date: string; from: StopRef; to: StopRef; seats: SeatRef[];
}
export interface CancelResponse {
  request_id: string; pnr: string; cancel_id: string; cancelled_seqs: number[]; booking_status: string;
  passengers: PassengerSeat[]; refund_estimate: Money; refund_status: string;
}
export interface Page<T> { items: T[]; next_cursor?: string }

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details: Record<string, unknown> = {},
    public requestId?: string,
  ) {
    super(message);
  }
  field(name: string): string | undefined {
    const f = this.details.fields as Record<string, string> | undefined;
    return f?.[name];
  }
}

// ---- session

interface Session { access: string; refresh: string; expiresAt: number; user: UserInfo }
const KEY = "rail.session";
type Listener = (u: UserInfo | null) => void;
const listeners = new Set<Listener>();

function load(): Session | null {
  try {
    const raw = localStorage.getItem(KEY);
    return raw ? (JSON.parse(raw) as Session) : null;
  } catch {
    return null;
  }
}
let session: Session | null = load();

function save(s: Session | null) {
  session = s;
  try {
    if (s) localStorage.setItem(KEY, JSON.stringify(s));
    else localStorage.removeItem(KEY);
  } catch {
    /* private mode: the session lives in memory only */
  }
  listeners.forEach((l) => l(s?.user ?? null));
}

export function currentUser(): UserInfo | null { return session?.user ?? null; }
export function onSession(l: Listener): () => void { listeners.add(l); return () => listeners.delete(l); }

export function deviceId(): string {
  try {
    let id = localStorage.getItem("rail.device");
    if (!id) {
      id = crypto.randomUUID();
      localStorage.setItem("rail.device", id);
    }
    return id;
  } catch {
    return "browser";
  }
}

function adopt(t: TokenResponse) {
  save({ access: t.access_token, refresh: t.refresh_token, expiresAt: Date.now() + t.expires_in * 1000, user: t.user });
}

let refreshing: Promise<void> | null = null;
async function refreshNow(): Promise<void> {
  if (!session) throw new ApiError(401, "UNAUTHENTICATED", "Sign in to continue");
  refreshing ??= (async () => {
    try {
      const t = await raw<TokenResponse>("POST", "/v1/auth/refresh", { refresh_token: session!.refresh, device_id: deviceId() });
      adopt(t);
    } catch (e) {
      save(null);
      throw e;
    } finally {
      refreshing = null;
    }
  })();
  return refreshing;
}

// ---- requests

interface Opts { headers?: Record<string, string>; auth?: boolean; signal?: AbortSignal }

type ErrorEnvelope = { error?: { code: string; message: string; request_id?: string; details?: Record<string, unknown> } };

/** Builds an ApiError from the backend's error envelope (§9.5), or from the bare HTTP status. */
function toApiError(res: Response, data: unknown): ApiError {
  const err = (data as ErrorEnvelope | undefined)?.error;
  return new ApiError(res.status, err?.code ?? "HTTP_" + res.status, err?.message ?? (res.statusText || `HTTP ${res.status}`),
    err?.details ?? {}, err?.request_id ?? res.headers.get("X-Request-Id") ?? undefined);
}

async function send<T>(method: string, path: string, body?: unknown, opts: Opts = {}): Promise<{ status: number; data: T }> {
  const headers: Record<string, string> = { ...opts.headers };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (opts.auth && session) headers.Authorization = `Bearer ${session.access}`;
  let res: Response;
  try {
    res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal: opts.signal });
  } catch (e) {
    if ((e as Error).name === "AbortError") throw e;
    throw new ApiError(0, "NETWORK", "Can't reach the booking service. Check your connection and try again.");
  }
  if (res.status === 204) return { status: 204, data: undefined as T };
  const text = await res.text();
  let data: unknown = undefined;
  try {
    data = text ? JSON.parse(text) : undefined;
  } catch {
    /* non-JSON (e.g. proxy error page) */
  }
  // 202 Accepted carries {request_id, poll_url, stream_url}: not an error.
  if (!res.ok && res.status !== 202) throw toApiError(res, data);
  return { status: res.status, data: data as T };
}

async function raw<T>(method: string, path: string, body?: unknown, opts: Opts = {}): Promise<T> {
  return (await send<T>(method, path, body, opts)).data;
}

/** Unauthenticated GET for the public read path (stations, search). */
export function publicGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  return raw<T>("GET", path, undefined, { signal });
}

/** Runs an authenticated call: refresh ahead of expiry, retry once on TOKEN_EXPIRED. */
async function authed<R>(call: () => Promise<R>): Promise<R> {
  if (session && session.expiresAt - Date.now() < 120_000) {
    await refreshNow().catch(() => undefined);
  }
  try {
    return await call();
  } catch (e) {
    if (e instanceof ApiError && e.code === "TOKEN_EXPIRED" && session) {
      await refreshNow();
      return call();
    }
    if (e instanceof ApiError && (e.code === "UNAUTHENTICATED" || e.code === "REFRESH_REUSED")) save(null);
    throw e;
  }
}

/** Calls the API with the user's token, refreshing it when needed. */
export function api<T>(method: string, path: string, body?: unknown, opts: Opts = {}): Promise<T> {
  return authed(() => raw<T>(method, path, body, { ...opts, auth: true }));
}

/** Same as api() but also returns the HTTP status (for 201 vs 202). */
export function apiWithStatus<T>(method: string, path: string, body?: unknown, opts: Opts = {}): Promise<{ status: number; data: T }> {
  return authed(() => send<T>(method, path, body, { ...opts, auth: true }));
}

export const idemKey = () => crypto.randomUUID();

// ---- auth

export async function login(loginId: string, password: string, captcha?: string) {
  const t = await raw<TokenResponse>("POST", "/v1/auth/login", { login: loginId, password, device_id: deviceId(), captcha_token: captcha || undefined });
  adopt(t);
  return t.user;
}

export async function register(r: { phone: string; email?: string; name: string; nid?: string; password: string }, captcha?: string) {
  return raw<RegisterResponse>("POST", "/v1/auth/register", { ...r, captcha_token: captcha || undefined }, { headers: { "Idempotency-Key": idemKey() } });
}

/** Which CAPTCHA register and login need; fetched once. A failed fetch means none (the server still decides). */
let captchaCfg: Promise<{ provider?: string; site_key?: string }> | null = null;
export function captchaConfig() {
  captchaCfg ??= publicGet<{ provider?: string; site_key?: string }>("/v1/auth/captcha").catch(() => {
    captchaCfg = null;
    return {};
  });
  return captchaCfg;
}

export async function logout() {
  const s = session;
  if (s) await api("POST", "/v1/auth/logout", { refresh_token: s.refresh }).catch(() => undefined);
  save(null);
}

/** Downloads the PDF ticket (it needs the bearer token, so no plain link). */
export async function downloadTicket(pnr: string) {
  const res = await authed(async () => {
    let r: Response;
    try {
      r = await fetch(`/v1/bookings/${pnr}/ticket`, { headers: { Authorization: `Bearer ${session?.access ?? ""}` } });
    } catch {
      throw new ApiError(0, "NETWORK", "Can't reach the booking service. Check your connection and try again.");
    }
    if (!r.ok) {
      const e = toApiError(r, await r.json().catch(() => undefined));
      if (e.code === "NOT_FOUND") e.message = "The ticket isn't available for this booking.";
      throw e;
    }
    return r;
  });
  const url = URL.createObjectURL(await res.blob());
  const a = document.createElement("a");
  a.href = url;
  a.download = `ticket-${pnr}.pdf`;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

// ---- waiting-room admission token, kept per journey for this tab

const admKey = (j: number) => `rail.admission.${j}`;
export function admissionToken(journeyId: number): string | null {
  try {
    const raw = sessionStorage.getItem(admKey(journeyId));
    if (!raw) return null;
    const { token, exp } = JSON.parse(raw) as { token: string; exp: number };
    return exp - Date.now() > 30_000 ? token : null;
  } catch {
    return null;
  }
}
/** When the stored admission expires (ms since epoch), 0 if there is none. */
export function admissionExpiry(journeyId: number): number {
  try {
    const raw = sessionStorage.getItem(admKey(journeyId));
    return raw ? (JSON.parse(raw) as { exp: number }).exp : 0;
  } catch {
    return 0;
  }
}
export function storeAdmission(journeyId: number, token: string, expiresAt: string) {
  try {
    sessionStorage.setItem(admKey(journeyId), JSON.stringify({ token, exp: Date.parse(expiresAt) }));
  } catch {
    /* ignore */
  }
}
export function dropAdmission(journeyId: number) {
  try {
    sessionStorage.removeItem(admKey(journeyId));
  } catch {
    /* ignore */
  }
}

/**
 * Follows a 202 (no decision within 2 s) through GET /v1/requests/{id} until
 * the allocator answers; returns the status and body the call would have had.
 */
export async function settle<T>(r: { status: number; data: T | AcceptedResponse }): Promise<{ status: number; data: T }> {
  if (r.status !== 202) return r as { status: number; data: T };
  const id = (r.data as AcceptedResponse).request_id;
  for (let i = 0; i < 30; i++) {
    await new Promise((res) => setTimeout(res, 1000));
    const st = await api<RequestStatus>("GET", `/v1/requests/${id}`);
    if (st.state === "DONE") {
      if ((st.status_code ?? 500) >= 400) {
        const err = (st.body as ErrorEnvelope | undefined)?.error;
        throw new ApiError(st.status_code ?? 409, err?.code ?? "HTTP_" + st.status_code, err?.message ?? "request failed",
          err?.details ?? {}, err?.request_id);
      }
      return { status: st.status_code ?? 200, data: st.body as T };
    }
  }
  throw new ApiError(504, "SLOW", "Still waiting for an answer. Check again in a minute.");
}

// ---- live seat deltas (GET /v1/journeys/{id}/seatmap/live, SSE)

export type LiveEvent = { type: "delta"; delta: SeatDelta } | { type: "resync" } | { type: "reconnect"; reason?: string };

/**
 * Streams seat deltas after `version`. Any logged-in user may watch; admission
 * is only needed to lock seats. EventSource cannot send the bearer header, so
 * this reads the SSE stream with fetch. Resolves when the server closes the
 * stream; throws on HTTP errors.
 */
export async function streamSeatMap(
  journeyId: number, version: number, onEvent: (e: LiveEvent) => void, signal: AbortSignal,
): Promise<void> {
  if (session && session.expiresAt - Date.now() < 120_000) await refreshNow().catch(() => undefined);
  const res = await fetch(`/v1/journeys/${journeyId}/seatmap/live`, {
    headers: {
      Authorization: `Bearer ${session?.access ?? ""}`, "Last-Event-ID": String(version), Accept: "text/event-stream",
    },
    signal,
  });
  if (!res.ok || !res.body) {
    const e = toApiError(res, await res.json().catch(() => undefined));
    if (e.code === "UNAUTHENTICATED" || e.code === "REFRESH_REUSED") save(null);
    throw e;
  }
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += value;
    let cut: number;
    while ((cut = buf.indexOf("\n\n")) >= 0) {
      const frame = buf.slice(0, cut);
      buf = buf.slice(cut + 2);
      let event = "message";
      let data = "";
      for (const line of frame.split("\n")) {
        if (line.startsWith("event: ")) event = line.slice(7);
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      if (event === "delta") onEvent({ type: "delta", delta: JSON.parse(data) as SeatDelta });
      else if (event === "resync") onEvent({ type: "resync" });
      else if (event === "reconnect") {
        let reason: string | undefined;
        try {
          reason = (JSON.parse(data || "{}") as { reason?: string }).reason;
        } catch {
          /* no reason given */
        }
        onEvent({ type: "reconnect", reason });
      }
    }
  }
}
