import type { Coach } from "../api";
import { seatTypeAbbr, seatTypeName } from "../format";
import { num, t, type Key } from "../i18n";
import { isFree, takenOnSegment, type DecodedMap } from "../seatmap";

export type SeatState = "free" | "mine" | "pending" | "taken" | "other";

interface Props {
  coach: Coach;
  map: DecodedMap | null;
  poolIndex: number;
  from: number;
  to: number;
  tripMask: bigint;
  mine: Set<string>;
  pending: Set<string>;
  onClick: (seatId: string, state: SeatState) => void;
  stopCodes: string[];
  vertical: boolean;
}

export function seatState(map: DecodedMap | null, seatIdx: number, poolIndex: number, m: bigint, mine: boolean, pending: boolean): SeatState {
  if (pending) return "pending";
  if (mine) return "mine";
  if (!map || map.pool[seatIdx] !== poolIndex) return "other";
  return isFree(map.bits[seatIdx] ?? 0n, m) ? "free" : "taken";
}

const stateText: Record<SeatState, Key> = {
  free: "seat.free",
  mine: "seat.mine",
  pending: "seat.pending",
  taken: "seat.taken",
  other: "seat.other",
};

/**
 * Top view of one coach. Every seat carries a strip with one cell per stretch
 * of the route: a seat can be taken for part of the journey and still be free
 * for yours. Selected, held and sold seats look the same to everyone else.
 */
export default function CoachMap({ coach, map, poolIndex, from, to, tripMask, mine, pending, onClick, stopCodes, vertical }: Props) {
  const segs = map?.segments ?? Math.max(stopCodes.length - 1, 1);
  // On wide screens the coach lies along the page as seen from above (its
  // length, x, runs across). On phones it stands up like a bus seat plan, so a
  // whole coach fits without sideways scrolling. Lines without a seat are gaps
  // in the drawing (the aisle, the space between cabins).
  const usedX = new Set(coach.seats.map((s) => s.x));
  const usedY = new Set(coach.seats.map((s) => s.y));
  const tracks = (n: number, used: Set<number>, seat: string, gap: string) =>
    Array.from({ length: n }, (_, i) => (used.has(i) ? seat : gap)).join(" ");
  const grid = vertical
    ? {
        // phones: seat columns share the width, so seats fill the screen
        gridTemplateColumns: tracks(coach.height, usedY, "minmax(0, 1fr)", "minmax(0.75rem, 0.5fr)"),
        gridTemplateRows: tracks(coach.width, usedX, "auto", "0.75rem"),
      }
    : {
        // seats share the page width, never narrower than --berth-w (then the coach scrolls)
        gridTemplateColumns: tracks(coach.width, usedX, "minmax(var(--berth-w), 1fr)", "0.75rem"),
        gridTemplateRows: tracks(coach.height, usedY, "var(--berth-h)", "var(--aisle-w)"),
      };
  return (
    <div className={`coach-frame ${vertical ? "coach-v" : "coach-h"}`}>
      <div className="coach" style={grid}>
        {coach.seats.map((s) => {
          const st = seatState(map, s.seat_idx, poolIndex, tripMask, mine.has(s.seat_id), pending.has(s.seat_id));
          const bits = map?.bits[s.seat_idx] ?? 0n;
          const label = `${s.seat_id}, ${seatTypeName(s.seat_type)}, ${t(stateText[st])}`;
          return (
            <button
              key={s.seat_id}
              type="button"
              className={`berth b-${st}`}
              style={vertical ? { gridColumn: s.y + 1, gridRow: s.x + 1 } : { gridColumn: s.x + 1, gridRow: s.y + 1 }}
              disabled={st === "taken" || st === "other" || st === "pending"}
              aria-pressed={st === "mine"}
              aria-label={label}
              title={label}
              onClick={() => onClick(s.seat_id, st)}
            >
              <span className="berth-no">{num(s.seat_no)}</span>
              <span className="berth-type">{seatTypeAbbr(s.seat_type)}</span>
              <span className="segstrip" aria-hidden="true">
                {Array.from({ length: segs }, (_, g) => {
                  const trip = g >= from && g < to;
                  // Your own lock fills your stretches: show them as your trip, not as taken.
                  const sold = takenOnSegment(bits, g) && !(trip && (st === "mine" || st === "pending"));
                  return <i key={g} className={`${sold ? "sold" : ""} ${trip ? "trip" : ""}`} />;
                })}
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

/** Explains the strip using the actual route. */
export function RouteKey({ stopCodes, from, to }: { stopCodes: string[]; from: number; to: number }) {
  return (
    <div className="routekey" aria-hidden="true">
      {stopCodes.slice(0, -1).map((c, i) => (
        <span key={c + i} className={i >= from && i < to ? "trip" : ""}>{c}–{stopCodes[i + 1]}</span>
      ))}
    </div>
  );
}
