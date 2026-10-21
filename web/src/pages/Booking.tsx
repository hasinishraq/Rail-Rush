import { useRef, useState, type FormEvent } from "react";
import { Link, useLocation, useParams } from "react-router-dom";
import {
  api, apiWithStatus, ApiError, downloadTicket, idemKey, settle,
  type AcceptedResponse, type BookingResponse, type CancelRequest, type CancelResponse, type CreatePaymentRequest,
  type HoldResponse, type OTPInfo, type PaymentResponse, type VerifyOTPResponse,
} from "../api";
import { className, countdown, dateTime, money, paymentMethods, statusText, ticketStatusText } from "../format";
import { num, t, tCode } from "../i18n";
import { stationName } from "../stations";
import { useNow, usePoll } from "../hooks";
import Steps from "../components/Steps";

const PAID = ["CONFIRMED", "PARTIALLY_CANCELLED"];
const HELD = ["PENDING_OTP", "PENDING_PAYMENT"];

function fromHold(h: HoldResponse): BookingResponse {
  return {
    pnr: h.pnr, status: h.status, journey_id: h.journey_id, train_no: "", train_name: h.train_name, run_date: "",
    from: h.from, to: h.to, class_code: h.class_code, passengers: h.passengers, amount: h.amount,
    hold_expires_at: h.expires_at, updated_at: new Date().toISOString(),
  };
}

export default function Booking() {
  const { pnr = "" } = useParams();
  const loc = useLocation();
  const hold = (loc.state as { hold?: HoldResponse } | null)?.hold;
  const now = useNow();
  const [b, setB] = useState<BookingResponse | null>(hold ? fromHold(hold) : null);
  const [otpInfo, setOtpInfo] = useState<OTPInfo | null>(hold?.otp ?? null);
  const [code, setCode] = useState("");
  const [method, setMethod] = useState("BKASH");
  const [error, setError] = useState("");
  const [info, setInfo] = useState("");
  const [notFound, setNotFound] = useState(false);
  const [busy, setBusy] = useState(false);
  const [cancelSel, setCancelSel] = useState<number[]>([]);
  const [cancelled, setCancelled] = useState<CancelResponse | null>(null);
  const payKey = useRef(idemKey());
  const cancelKey = useRef(idemKey());

  const reload = async () => {
    try {
      setB(await api<BookingResponse>("GET", `/v1/bookings/${pnr}`));
    } catch (e) {
      if ((e as ApiError).status === 404 && !hold) setNotFound(true);
    }
  };
  usePoll(reload, !b || HELD.includes(b.status) ? 2000 : 15000);

  async function verify(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const v = await api<VerifyOTPResponse>("POST", `/v1/holds/${pnr}/otp/verify`, { otp: code.trim() });
      setInfo(t("b.codeOk"));
      setCode("");
      setB((cur) => (cur ? { ...cur, status: v.status, hold_expires_at: v.expires_at } : cur));
      await reload();
    } catch (err) {
      const a = err as ApiError;
      setError(
        a.code === "OTP_INVALID" ? t("b.OTP_INVALID", { n: Number(a.details.attempts_left) }) :
        a.code === "OTP_EXPIRED" ? t("b.OTP_EXPIRED") :
        a.code === "OTP_LOCKED" ? t("b.OTP_LOCKED") :
        a.code === "HOLD_EXPIRED" ? t("b.HOLD_EXPIRED") : a.message,
      );
    } finally {
      setBusy(false);
    }
  }

  async function resend() {
    setError("");
    try {
      setOtpInfo(await api<OTPInfo>("POST", `/v1/holds/${pnr}/otp/resend`));
      setInfo(t("b.newCode"));
    } catch (err) {
      const a = err as ApiError;
      setError(a.code === "OTP_LOCKED" ? t("b.noNewCode", { s: Number(a.details.retry_after) }) : a.message);
    }
  }

  async function pay() {
    setBusy(true);
    setError("");
    try {
      const req: CreatePaymentRequest = { pnr, method, return_url: `${window.location.origin}/pay/return` };
      const p = await api<PaymentResponse>("POST", "/v1/payments", req, { headers: { "Idempotency-Key": payKey.current } });
      if (p.redirect_url) window.location.assign(p.redirect_url);
      else setInfo(t("b.alreadyOpened"));
    } catch (e) {
      const a = e as ApiError;
      payKey.current = idemKey();
      setError(
        a.code === "HOLD_EXPIRED" ? t("b.payHoldExpired") :
        a.code === "OTP_REQUIRED" ? t("b.OTP_REQUIRED") :
        a.code === "ALREADY_PAID" ? t("b.ALREADY_PAID") : a.message,
      );
    } finally {
      setBusy(false);
    }
  }

  async function cancel(seqs: number[]) {
    setBusy(true);
    setError("");
    try {
      const body: CancelRequest = seqs.length ? { passenger_seqs: seqs } : {};
      const r = await settle(await apiWithStatus<CancelResponse | AcceptedResponse>("POST", `/v1/bookings/${pnr}/cancel`,
        body, { headers: { "Idempotency-Key": cancelKey.current } }));
      cancelKey.current = idemKey();
      setCancelled(r.data);
      setCancelSel([]);
      await reload();
    } catch (e) {
      const a = e as ApiError;
      cancelKey.current = idemKey();
      setError(a.code === "NOT_CANCELLABLE" ? t("b.NOT_CANCELLABLE", { reason: String(a.details.reason ?? "status") }) : a.message);
    } finally {
      setBusy(false);
    }
  }

  if (notFound) return <p className="empty">{t("b.notFound", { pnr })} <Link to="/trips">{t("b.seeTrips")}</Link></p>;
  if (!b) return <p className="loading">{t("b.loading")}</p>;

  const expiresIn = b.hold_expires_at ? new Date(b.hold_expires_at).getTime() - now : 0;
  const isPaid = PAID.includes(b.status);
  const live = b.passengers.filter((p) => p.status === "ISSUED");
  const resendIn = otpInfo ? new Date(otpInfo.resend_after).getTime() - now : 0;

  return (
    <div className="booking">
      {(HELD.includes(b.status) || isPaid) && <Steps current={isPaid ? 4 : 3} />}
      <section className={`status-band sb-${b.status}`} aria-live="polite">
        <div>
          <span className="sb-label">{t("b.pnr", { pnr: b.pnr })}</span>
          <h1 className="sb-status">{statusText(b.status)}</h1>
        </div>
        {HELD.includes(b.status) && expiresIn > 0 && (
          <div className="sb-timer" role="timer" aria-label={t("b.heldFor", { left: countdown(expiresIn) })}>
            <span className="sb-clock">{countdown(expiresIn)}</span>
            <span>{t("b.minutesToPay")}</span>
          </div>
        )}
      </section>

      {b.status === "PENDING_OTP" && expiresIn > 0 && (
        <form className="pay-box otp-box" onSubmit={verify}>
          <h2>{t("b.otpTitle")}</h2>
          <p>{t("b.otpText", { to: otpInfo?.sent_to ?? t("b.yourPhone") })}</p>
          <div className="otp-row">
            <label className="field">
              <span>{t("b.code")}</span>
              <input className="otp-input" inputMode="numeric" autoComplete="one-time-code" pattern="[0-9]{6}" maxLength={6} required
                value={code} onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))} />
            </label>
            <button className="btn btn-primary" disabled={busy || code.length !== 6}>{busy ? t("b.checking") : t("b.confirmCode")}</button>
          </div>
          <p className="fineprint">
            {t("b.noSms")}{" "}
            {resendIn > 0 ? t("b.resendIn", { s: Math.ceil(resendIn / 1000) }) : <button type="button" className="linkish" onClick={resend}>{t("b.resend")}</button>}
          </p>
        </form>
      )}

      {b.status === "PENDING_PAYMENT" && (
        <section className="pay-box">
          {expiresIn > 30_000 ? (
            <>
              <h2>{t("b.payTitle")}</h2>
              <p>{t("b.payText", { amount: money(b.amount) })}</p>
              <fieldset className="methods">
                <legend className="sr-only">{t("b.method")}</legend>
                {paymentMethods().map((m) => (
                  <label key={m.code} className={`method ${method === m.code ? "on" : ""}`}>
                    <input type="radio" name="method" value={m.code} checked={method === m.code} onChange={() => setMethod(m.code)} />
                    <span className="method-name">{m.name}</span>
                    <span className="method-kind">{m.kind}</span>
                  </label>
                ))}
              </fieldset>
              <div className="actions">
                <button className="btn btn-primary" disabled={busy} onClick={pay}>{busy ? t("b.opening") : t("b.pay", { amount: money(b.amount) })}</button>
              </div>
              <p className="fineprint">{t("b.payFine")}</p>
            </>
          ) : expiresIn > 0 ? (
            <p>{t("b.tooLate")}</p>
          ) : (
            <p>{t("b.ended")}</p>
          )}
        </section>
      )}

      {info && <p className="notice" role="status">{info}</p>}
      {b.seats_changed && isPaid && (
        <p className="notice" role="status">{t("b.seatsChanged")}</p>
      )}

      <section className="ticket">
        <div className="ticket-route">
          <div>
            <span className="tr-code">{b.from.station_code}</span>
            <span className="tr-name">{stationName(b.from.station_code, b.from.station_name)}</span>
            <span className="tr-time">{dateTime(b.from.time)}</span>
          </div>
          <div className="tr-train">
            {b.train_no && <span className="train-no">{num(b.train_no)}</span>}
            <span>{b.train_name}</span>
            <span className="muted">{className(b.class_code)}</span>
          </div>
          <div className="tr-end">
            <span className="tr-code">{b.to.station_code}</span>
            <span className="tr-name">{stationName(b.to.station_code, b.to.station_name)}</span>
            <span className="tr-time">{dateTime(b.to.time)}</span>
          </div>
        </div>

        <table className="pax-table">
          <caption className="sr-only">{t("b.passengers")}</caption>
          <thead>
            <tr>
              {isPaid && live.length > 1 && <th scope="col"><span className="sr-only">{t("b.selectCancel")}</span></th>}
              <th scope="col">{t("b.colPax")}</th><th scope="col">{t("b.colSeat")}</th><th scope="col">{t("b.colFare")}</th><th scope="col">{t("b.colStatus")}</th>
            </tr>
          </thead>
          <tbody>
            {b.passengers.map((p) => (
              <tr key={p.seq} className={p.status === "CANCELLED" ? "row-can" : ""}>
                {isPaid && live.length > 1 && (
                  <td>
                    {p.status === "ISSUED" && (
                      <input type="checkbox" aria-label={t("b.cancelX", { name: p.name })} checked={cancelSel.includes(p.seq)}
                        onChange={(e) => setCancelSel(e.target.checked ? [...cancelSel, p.seq] : cancelSel.filter((s) => s !== p.seq))} />
                    )}
                  </td>
                )}
                <td>{num(p.seq)}. {p.name}{p.type === "CHILD" ? ` ${t("b.child")}` : ""}</td>
                <td>{p.seat.seat_id}</td>
                <td>{money(p.fare)}</td>
                <td><span className={`pill ts-${p.status}`}>{ticketStatusText(p.status)}</span></td>
              </tr>
            ))}
          </tbody>
        </table>

        <dl className="facts">
          <div><dt>{t("b.total")}</dt><dd>{money(b.amount)}</dd></div>
          {b.payment && <div><dt>{t("b.payment")}</dt><dd>{tCode("pm", b.payment.method)}, {b.payment.status === "CAPTURED" ? t("b.paid") : b.payment.status.toLowerCase().replace("_", " ")}</dd></div>}
        </dl>
        <p className="fineprint">{t("b.totalFine")}</p>
      </section>

      {isPaid && (
        <section className="after-actions">
          <button className="btn btn-primary" onClick={() => downloadTicket(pnr).catch((e) => setError((e as Error).message))}>{t("b.download")}</button>
          <button className="btn btn-danger" disabled={busy || (live.length > 1 && cancelSel.length === 0)}
            onClick={() => cancel(live.length > 1 ? cancelSel : [])}>
            {busy ? t("b.cancelling") : live.length > 1 ? (cancelSel.length ? t("b.cancelSelected", { n: cancelSel.length }) : t("b.cancelNoneSelected")) : t("b.cancelBooking")}
          </button>
          {live.length > 1 && cancelSel.length === 0 && <span className="muted">{t("b.tickToCancel")}</span>}
        </section>
      )}

      {cancelled && (
        <p className="notice" role="status">
          {t("b.cancelled", { list: num(cancelled.cancelled_seqs.join(", ")) })}{" "}
          {cancelled.refund_estimate.amount_minor > 0 ? t("b.refundEst", { amount: money(cancelled.refund_estimate) }) : t("b.noRefund")}
        </p>
      )}
      {error && <p className="error" role="alert">{error}</p>}

      {b.refunds && b.refunds.length > 0 && (
        <section>
          <h2>{t("b.refunds")}</h2>
          <ul className="plain">
            {b.refunds.map((r) => (
              <li key={r.refund_id}>
                {money(r.amount)}, {r.reason === "CANCELLED" ? t("b.rCancel") : r.reason === "CONFIRM_FAILED" ? t("b.rFailed") : r.reason.toLowerCase().replace(/_/g, " ")}:{" "}
                {r.status === "SUCCEEDED" ? t("b.rDone") : r.status === "FAILED" ? t("b.rRetry") : t("b.rProgress")}
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}
