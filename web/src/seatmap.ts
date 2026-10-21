import type { SeatDelta, SeatMapResponse } from "./api";

/** Decoded seat map: one segment bitmap per seat, and its pool index (255 = not sellable). */
export interface DecodedMap {
  version: number;
  segments: number;
  bits: bigint[];
  pool: Uint8Array;
}

function b64(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export function decodeSeatMap(sm: SeatMapResponse): DecodedMap {
  const raw = b64(sm.seats);
  const view = new DataView(raw.buffer);
  const bits: bigint[] = [];
  for (let i = 0; i + 8 <= raw.length; i += 8) bits.push(view.getBigUint64(i, true));
  return { version: sm.version, segments: sm.segments, bits, pool: b64(sm.seat_pool) };
}

/**
 * Applies a live delta (§11.7). A delta covers versions (from_v, v] and carries
 * the final bits of every seat changed in that range, so it applies to a map at
 * version h when from_v <= h < v. Returns the same map if the delta is old, and
 * null on a gap (from_v > h): the caller refetches the snapshot.
 */
export function applyDelta(map: DecodedMap, d: SeatDelta): DecodedMap | null {
  if (d.v <= map.version) return map;
  if (d.from_v > map.version) return null;
  const raw = b64(d.changes);
  const view = new DataView(raw.buffer);
  const bits = map.bits.slice();
  for (let i = 0; i + 12 <= raw.length; i += 12) {
    const idx = view.getUint32(i, true);
    if (idx < bits.length) bits[idx] = view.getBigUint64(i + 4, true);
  }
  return { ...map, version: d.v, bits };
}

/** Bits from..to-1 set: the stretches a passenger travels. */
export const mask = (from: number, to: number) => ((1n << BigInt(to - from)) - 1n) << BigInt(from);

export const isFree = (bits: bigint, m: bigint) => (bits & m) === 0n;

export const takenOnSegment = (bits: bigint, seg: number) => ((bits >> BigInt(seg)) & 1n) === 1n;
