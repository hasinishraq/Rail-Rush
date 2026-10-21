import type { Money } from "./api";
import { lang, locale, num, t, tCode } from "./i18n";

// Bangladesh Railway: every station keeps Bangladesh Standard Time.
const TZ = "Asia/Dhaka";

export const money = (m?: Money) =>
  m
    ? new Intl.NumberFormat(lang() === "bn" ? "bn-BD" : "en-BD", {
        style: "currency", currency: m.currency || "BDT", currencyDisplay: "narrowSymbol",
        maximumFractionDigits: m.amount_minor % 100 ? 2 : 0,
      }).format(m.amount_minor / 100)
    : "";

/** Hour (0-23) and minute in Bangladesh. */
function clock(iso: string): [number, number] {
  const p = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: TZ })
    .formatToParts(new Date(iso));
  return [Number(p.find((x) => x.type === "hour")?.value), Number(p.find((x) => x.type === "minute")?.value)];
}

/** Bangla names for the parts of the day, the way times are said aloud: সকাল ৬:১৫, রাত ১১:১৫. */
function bnPeriod(h: number) {
  if (h >= 4 && h < 6) return "ভোর";
  if (h >= 6 && h < 12) return "সকাল";
  if (h >= 12 && h < 15) return "দুপুর";
  if (h >= 15 && h < 18) return "বিকেল";
  if (h >= 18 && h < 20) return "সন্ধ্যা";
  return "রাত";
}

/** 12-hour clock ("6:15 AM", "সকাল ৬:১৫"): what most travellers read on tickets and phones. */
export function time(iso: string) {
  const [h, m] = clock(iso);
  const h12 = h % 12 || 12, mm = String(m).padStart(2, "0");
  return lang() === "bn" ? `${bnPeriod(h)} ${num(h12)}:${num(mm)}` : `${h12}:${mm} ${h < 12 ? "AM" : "PM"}`;
}

export const day = (iso: string) =>
  new Intl.DateTimeFormat(locale(), { weekday: "short", day: "numeric", month: "short", timeZone: TZ }).format(new Date(iso));

export const dateTime = (iso: string) => `${day(iso)}, ${time(iso)}`;

export const duration = (min: number) => {
  const h = Math.floor(min / 60), m = min % 60;
  return m ? t("dur.hm", { h, m }) : t("dur.h", { h });
};

export function countdown(ms: number) {
  if (ms <= 0) return num("0:00");
  const s = Math.floor(ms / 1000);
  return num(`${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`);
}

/** Class names, a shorter form for tight spaces, and one plain line for people who don't know them. */
export const className = (code: string) => tCode("class", code);
export const classShort = (code: string) => (code === "SNIGDHA" ? t("classShort.SNIGDHA") : className(code));
export const classHint = (code: string) => {
  const h = tCode("classHint", code);
  return h === code ? "" : h;
};

export const seatTypeName = (type: string) => tCode("seatType", type);
export const seatTypeAbbr = (type: string) => tCode("seatAbbr", type);

/** Payment methods (§16.1), in the order Bangladeshi travellers use them most. */
const METHODS: { code: string; kind: "wallet" | "card" | "debit" }[] = [
  { code: "BKASH", kind: "wallet" },
  { code: "NAGAD", kind: "wallet" },
  { code: "ROCKET", kind: "wallet" },
  { code: "UPAY", kind: "wallet" },
  { code: "VISA", kind: "card" },
  { code: "MASTERCARD", kind: "card" },
  { code: "NEXUS", kind: "debit" },
];
export const paymentMethods = () =>
  METHODS.map((m) => ({ code: m.code, name: tCode("pm", m.code), kind: t(`pm.${m.kind}`) }));

/** Bangladesh Railway sells at most this many seats per user per journey. */
export const MAX_SEATS = 4;

/** Plain-language booking and ticket status. */
export const statusText = (status: string) => tCode("bs", status);
export const ticketStatusText = (status: string) => tCode("ts", status);

/** YYYY-MM-DD in Bangladesh, `days` from today. */
export const dhakaDate = (days = 0) =>
  new Intl.DateTimeFormat("en-CA", { timeZone: TZ }).format(new Date(Date.now() + days * 86_400_000));

export const tomorrow = () => dhakaDate(1);

/** Labels for the day picker: "Today", "Tomorrow", then "Thu" + "23 Oct". */
export function dayLabel(ymd: string): { top: string; bottom: string } {
  const d = new Date(ymd + "T06:00:00Z");
  const date = new Intl.DateTimeFormat(locale(), { day: "numeric", month: "short", timeZone: TZ }).format(d);
  if (ymd === dhakaDate(0)) return { top: t("day.today"), bottom: date };
  if (ymd === dhakaDate(1)) return { top: t("day.tomorrow"), bottom: date };
  return { top: new Intl.DateTimeFormat(locale(), { weekday: "short", timeZone: TZ }).format(d), bottom: date };
}
