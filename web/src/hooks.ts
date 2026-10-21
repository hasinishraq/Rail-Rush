import { useEffect, useRef, useState } from "react";
import { currentUser, onSession, type UserInfo } from "./api";

export function useUser(): UserInfo | null {
  const [u, setU] = useState(currentUser());
  useEffect(() => onSession(setU), []);
  return u;
}

/** True while the media query matches; follows window resizes. */
export function useMedia(query: string): boolean {
  const [match, setMatch] = useState(() => typeof matchMedia !== "undefined" && matchMedia(query).matches);
  useEffect(() => {
    const m = matchMedia(query);
    const on = () => setMatch(m.matches);
    on();
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, [query]);
  return match;
}

/** Ticks every second; used for hold countdowns. */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

/** Runs fn now and every ms while enabled; pauses while the tab is hidden. */
export function usePoll(fn: () => void | Promise<void>, ms: number, enabled = true) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    if (!enabled) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    const tick = async () => {
      if (!document.hidden) {
        try {
          await ref.current();
        } catch {
          /* the caller shows errors */
        }
      }
      if (!stopped) timer = setTimeout(tick, ms);
    };
    tick();
    return () => {
      stopped = true;
      clearTimeout(timer);
    };
  }, [ms, enabled]);
}
