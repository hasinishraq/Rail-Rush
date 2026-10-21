// Every station once (24 today, cached for an hour by the API), so pages that
// only get a station code and English name from the API can show the Bangla name.
import { publicGet, type Station } from "./api";
import { lang } from "./i18n";

let byCode = new Map<string, Station>();

export async function loadStations(): Promise<void> {
  const d = await publicGet<{ items: Station[] }>("/v1/stations?limit=50&q=").catch(() => null);
  if (d) byCode = new Map(d.items.map((s) => [s.code, s]));
}

/** The station's name in the reader's language; `fallback` (the API's English name) until the list has loaded. */
export function stationName(code: string, fallback: string): string {
  const s = byCode.get(code);
  if (!s) return fallback;
  return lang() === "bn" && s.name_bn ? s.name_bn : s.name;
}
