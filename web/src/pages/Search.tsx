import { useEffect, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { ApiError, publicGet, type ClassAvail, type SearchResponse, type Station, type TrainOption } from "../api";
import StationBoard, { localName } from "../components/StationBoard";
import Steps from "../components/Steps";
import { classHint, className, classShort, dateTime, day, dayLabel, dhakaDate, duration, money, time, tomorrow } from "../format";
import { num, t } from "../i18n";
import { stationName } from "../stations";

const statusLabel = (c: ClassAvail) => {
  switch (c.status) {
    case "AVAILABLE": return t("avail.left", { n: c.count });
    case "SOLD_OUT": return t("avail.soldOut");
    case "CLOSED": return t("avail.closed");
    case "NOT_OPEN": return c.opens_at ? t("avail.opens", { when: dateTime(c.opens_at) }) : t("avail.notOpen");
  }
  return c.status;
};

/** Sales open 10 days ahead, so today plus the next 10 days covers every bookable train. */
const BOOKABLE_DAYS = 11;

/** "Parjotak Express (816)" -> "Parjotak Express": the number is shown on its own line. */
const trainName = (name: string) => name.replace(/\s*\(\d+\)$/, "");

async function stationByCode(code: string): Promise<Station | null> {
  const d = await publicGet<{ items: Station[] }>(`/v1/stations?limit=5&q=${encodeURIComponent(code)}`).catch(() => null);
  return d?.items.find((s) => s.code === code) ?? null;
}

export default function Search() {
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const [from, setFrom] = useState<Station | null>(null);
  const [to, setTo] = useState<Station | null>(null);
  const [date, setDate] = useState(params.get("date") ?? tomorrow());
  const [result, setResult] = useState<SearchResponse | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const days = Array.from({ length: BOOKABLE_DAYS }, (_, i) => dhakaDate(i));
  if (!days.includes(date)) days.unshift(date);

  // Restore a search from the URL (back button, shared link).
  useEffect(() => {
    const f = params.get("from"), to = params.get("to");
    if (f) stationByCode(f).then(setFrom);
    if (to) stationByCode(to).then(setTo);
    if (f && to && params.get("date")) run(f, to, params.get("date")!);
  }, []);

  async function run(f: string, dest: string, d: string) {
    setBusy(true);
    setError("");
    try {
      const q = new URLSearchParams({ from: f, to: dest, date: d });
      setResult(await publicGet<SearchResponse>(`/v1/search?${q}`));
    } catch (e) {
      setResult(null);
      const a = e as ApiError;
      setError(
        a.status === 429 ? t("search.err429") :
        a.code === "VALIDATION_FAILED" ? Object.values((a.details.fields as Record<string, string>) ?? {}).join(". ") || a.message :
        a.message,
      );
    } finally {
      setBusy(false);
    }
  }

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!from || !to) return setError(!from ? t("search.errFrom") : t("search.errTo"));
    if (from.code === to.code) return setError(t("search.errSame"));
    setParams({ from: from.code, to: to.code, date }, { replace: true });
    run(from.code, to.code, date);
  };

  const swap = () => {
    setFrom(to);
    setTo(from);
  };

  const book = (tr: TrainOption, c: ClassAvail) =>
    navigate(`/journey/${tr.journey_id}?from=${tr.from.idx}&to=${tr.to.idx}&class=${c.class_code}`);

  const fromName = from ? localName(from) : result?.from ?? "";
  const toName = to ? localName(to) : result?.to ?? "";

  return (
    <>
      <section className="hero" aria-labelledby="hero-title">
        <div className="hero-inner">
          <h1 id="hero-title" className="sr-only">{t("search.title")}</h1>
          <Steps current={0} />
        </div>
      </section>
      <form className="search" onSubmit={submit} aria-label={t("app.searchTrains")}>
        <div className="boards">
          <StationBoard label={t("search.from")} value={from} onChange={setFrom} />
          <button type="button" className="swap" onClick={swap} aria-label={t("search.swap")} title={t("search.swap")}>⇄</button>
          <StationBoard label={t("search.to")} value={to} onChange={setTo} />
        </div>
        <fieldset className="days">
          <legend>{t("search.day")}</legend>
          <div className="day-strip">
            {days.map((d) => {
              const l = dayLabel(d);
              return (
                <label key={d} className={`day ${d === date ? "on" : ""}`}>
                  <input type="radio" name="date" value={d} checked={d === date} onChange={() => setDate(d)} />
                  <span className="day-top">{l.top}</span>
                  <span className="day-bottom">{l.bottom}</span>
                </label>
              );
            })}
          </div>
        </fieldset>
        {error && <p className="error" role="alert">{error}</p>}
        <button className="btn btn-primary btn-big" disabled={busy}>{busy ? t("search.searching") : t("search.show")}</button>
        <p className="fineprint">{t("search.salesNote")}</p>
      </form>

      {result && (
        <section className="results" aria-live="polite" aria-labelledby="results-title">
          <h2 id="results-title" className="results-title">
            {result.trains.length === 1
              ? t("results.title1", { from: fromName, to: toName })
              : t("results.titleN", { n: result.trains.length, from: fromName, to: toName })}
            <span className="results-day">{day(result.date + "T06:00:00Z")}</span>
          </h2>
          {result.trains.length === 0 ? (
            <div className="empty-card">
              <p>{t("results.none")}</p>
              <p className="muted">{t("results.noneHint")}</p>
            </div>
          ) : (
            <>
              <p className="results-help">{t("results.help")}</p>
              <ol className="timetable">
                {result.trains.map((tr) => (
                  <li key={tr.journey_id} className="train-row">
                    <div className="train-id">
                      <span className="train-name">{trainName(tr.train_name)}</span>
                      <span className="train-no">{t("train.no", { n: num(tr.train_no) })}</span>
                    </div>
                    <div className="train-times">
                      <div className="tt-stop">
                        <span className="tt-label">{t("train.leaves")}</span>
                        <span className="t-big">{time(tr.from.time)}</span>
                        <span className="t-place">{stationName(tr.from.station_code, tr.from.station_name)}</span>
                      </div>
                      <span className="t-line"><span>{duration(tr.duration_min)}</span></span>
                      <div className="tt-stop tt-end">
                        <span className="tt-label">{t("train.arrives")}</span>
                        <span className="t-big">{time(tr.to.time)}</span>
                        <span className="t-place">
                          {stationName(tr.to.station_code, tr.to.station_name)}
                          {day(tr.to.time) !== day(tr.from.time) && <b className="next-day"> {t("train.nextDay")}</b>}
                        </span>
                      </div>
                    </div>
                    <ul className="classes" aria-label={t("train.classes", { train: trainName(tr.train_name) })}>
                      {[...tr.classes].sort((a, b) => a.fare.amount_minor - b.fare.amount_minor).map((c) => {
                        const bookable = c.status === "AVAILABLE" || c.status === "NOT_OPEN";
                        return (
                          <li key={c.class_code}>
                            <button className={`class-pick st-${c.status}`} disabled={!bookable} onClick={() => book(tr, c)}
                              aria-label={`${className(c.class_code)}, ${money(c.fare)}, ${statusLabel(c)}`}>
                              <span className="cp-top">
                                <span className="cp-class">{classShort(c.class_code)}</span>
                                <span className="cp-fare">{money(c.fare)}</span>
                              </span>
                              {classHint(c.class_code) && <span className="cp-hint">{classHint(c.class_code)}</span>}
                              <span className="cp-status">{statusLabel(c)}</span>
                            </button>
                          </li>
                        );
                      })}
                      {tr.classes.length === 0 && <li className="muted">{t("train.noSeats")}</li>}
                    </ul>
                  </li>
                ))}
              </ol>
              <p className="fineprint">{t("results.fine")}</p>
            </>
          )}
        </section>
      )}
    </>
  );
}
