import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type BookingSummary, type Page } from "../api";
import { dateTime, statusText } from "../format";
import { num, t } from "../i18n";
import { stationName } from "../stations";

export default function Trips() {
  const [tab, setTab] = useState<"upcoming" | "past">("upcoming");
  const [items, setItems] = useState<BookingSummary[] | null>(null);
  const [cursor, setCursor] = useState<string | undefined>();
  const [error, setError] = useState("");

  const load = async (more = false) => {
    try {
      const q = `/v1/me/bookings?status=${tab}&limit=20${more && cursor ? `&cursor=${cursor}` : ""}`;
      const page = await api<Page<BookingSummary>>("GET", q);
      setItems((cur) => (more && cur ? [...cur, ...page.items] : page.items));
      setCursor(page.next_cursor);
    } catch (e) {
      setError((e as Error).message);
    }
  };

  useEffect(() => {
    setItems(null);
    load();
  }, [tab]);

  return (
    <div>
      <h1>{t("tr.title")}</h1>
      <div className="tabs" role="tablist">
        <button role="tab" aria-selected={tab === "upcoming"} className={tab === "upcoming" ? "active" : ""} onClick={() => setTab("upcoming")}>{t("tr.upcoming")}</button>
        <button role="tab" aria-selected={tab === "past"} className={tab === "past" ? "active" : ""} onClick={() => setTab("past")}>{t("tr.past")}</button>
      </div>
      {error && <p className="error">{error}</p>}
      {!items ? (
        <p className="loading">{t("tr.loading")}</p>
      ) : items.length === 0 ? (
        <div className="empty-card">
          {tab === "upcoming" ? (
            <>
              <p>{t("tr.none")}</p>
              <Link className="btn btn-primary" to="/">{t("nav.book")}</Link>
            </>
          ) : (
            <p>{t("tr.nonePast")}</p>
          )}
        </div>
      ) : (
        <ol className="trip-list">
          {items.map((b) => (
            <li key={b.pnr}>
              <Link to={`/bookings/${b.pnr}`} className="trip">
                <span className="trip-route">{t("tr.route", { from: stationName(b.from.station_code, b.from.station_name || b.from.station_code), to: stationName(b.to.station_code, b.to.station_name || b.to.station_code) })}</span>
                <span className="trip-when">{t("tr.when", { when: b.from.time && !b.from.time.startsWith("0001-") ? dateTime(b.from.time) : b.run_date, no: num(b.train_no) })}</span>
                <span className="trip-meta">{t("tr.meta", { pnr: b.pnr, seats: b.seats.map((s) => s.seat_id).join(", ") || t("tr.noSeat") })}</span>
                <span className={`pill sp-${b.status}`}>{statusText(b.status)}</span>
              </Link>
            </li>
          ))}
        </ol>
      )}
      {cursor && <button className="btn btn-quiet" onClick={() => load(true)}>{t("tr.more")}</button>}
    </div>
  );
}
