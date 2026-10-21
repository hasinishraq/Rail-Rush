import { useState } from "react";
import { Link, Navigate, useSearchParams } from "react-router-dom";
import { api, ApiError, type PaymentResponse } from "../api";
import { usePoll } from "../hooks";
import { t } from "../i18n";

const DONE = ["CONFIRMED", "PARTIALLY_CANCELLED", "CONFIRM_FAILED"];

/** Back from the gateway. The redirect proves nothing; the payment record does. */
export default function PayReturn() {
  const [params] = useSearchParams();
  const id = params.get("payment_id") ?? "";
  const [p, setP] = useState<PaymentResponse | null>(null);
  const [error, setError] = useState("");
  const [tries, setTries] = useState(0);

  const settled = p && (p.status === "FAILED" || (p.status === "CAPTURED" && DONE.includes(p.booking_status ?? "")));
  usePoll(
    async () => {
      setTries((t) => t + 1);
      try {
        setP(await api<PaymentResponse>("GET", `/v1/payments/${id}`));
      } catch (e) {
        setError((e as ApiError).status === 404 ? t("pr.notFound") : (e as Error).message);
      }
    },
    2000,
    !!id && !settled && tries < 30,
  );

  if (!id) return <Navigate to="/trips" replace />;
  if (settled && p!.status === "CAPTURED") return <Navigate to={`/bookings/${p!.pnr}`} replace />;

  return (
    <div className="narrow">
      {p?.status === "FAILED" ? (
        <>
          <h1>{t("pr.failTitle")}</h1>
          <p>{t("pr.failText")}</p>
          <Link className="btn btn-primary" to={`/bookings/${p.pnr}`}>{t("pr.back", { pnr: p.pnr })}</Link>
        </>
      ) : (
        <>
          <h1>{t("pr.title")}</h1>
          <p aria-live="polite">
            {p?.status === "CAPTURED" ? t("pr.received") : t("pr.waiting")}
          </p>
          {tries >= 30 && p && (
            <p>{t("pr.slow")} <Link to={`/bookings/${p.pnr}`}>{t("pr.check", { pnr: p.pnr })}</Link></p>
          )}
          {error && <p className="error" role="alert">{error}</p>}
        </>
      )}
    </div>
  );
}
