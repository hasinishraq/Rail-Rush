import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from "react";
import { captchaConfig } from "../api";
import { lang } from "../i18n";

interface TurnstileApi {
  render(el: HTMLElement, opts: Record<string, unknown>): string;
  reset(id: string): void;
  remove(id: string): void;
}
declare global {
  interface Window { turnstile?: TurnstileApi }
}

const SCRIPT = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
let scriptLoad: Promise<TurnstileApi> | null = null;

function loadScript(): Promise<TurnstileApi> {
  scriptLoad ??= new Promise((resolve, reject) => {
    if (window.turnstile) return resolve(window.turnstile);
    const s = document.createElement("script");
    s.src = SCRIPT;
    s.async = true;
    s.onload = () => (window.turnstile ? resolve(window.turnstile) : reject(new Error("turnstile missing")));
    s.onerror = () => {
      scriptLoad = null; // let the next mount try again
      reject(new Error("turnstile failed to load"));
    };
    document.head.appendChild(s);
  });
  return scriptLoad;
}

export interface CaptchaHandle {
  /** A fresh single-use token, waiting for the challenge if needed; undefined when the server has no CAPTCHA. */
  token(): Promise<string | undefined>;
}

/** Cloudflare Turnstile widget; renders nothing while the server has it switched off. */
const Turnstile = forwardRef<CaptchaHandle>(function Turnstile(_, ref) {
  const box = useRef<HTMLDivElement>(null);
  const widget = useRef<{ api: TurnstileApi; id: string } | null>(null);
  const ready = useRef<string | null>(null);
  const waiters = useRef<((tok: string) => void)[]>([]);
  const [enabled, setEnabled] = useState<boolean | null>(null);

  useEffect(() => {
    let gone = false;
    captchaConfig().then(async (c) => {
      if (gone) return;
      setEnabled(!!c.site_key);
      if (!c.site_key || !box.current) return;
      const api = await loadScript();
      if (gone || !box.current) return;
      const id = api.render(box.current, {
        sitekey: c.site_key,
        language: lang() === "en" ? "en" : "auto",
        callback: (tok: string) => {
          const w = waiters.current.shift();
          if (w) w(tok);
          else ready.current = tok;
        },
        "expired-callback": () => { ready.current = null; },
      });
      widget.current = { api, id };
    });
    return () => {
      gone = true;
      if (widget.current) widget.current.api.remove(widget.current.id);
      widget.current = null;
    };
  }, []);

  useImperativeHandle(ref, () => ({
    async token() {
      const c = await captchaConfig();
      if (!c.site_key) return undefined;
      const take = () => {
        const tok = ready.current;
        ready.current = null;
        // Tokens are single-use: start the next challenge straight away.
        if (widget.current) widget.current.api.reset(widget.current.id);
        return tok;
      };
      if (ready.current) return take() ?? undefined;
      return new Promise<string>((resolve) => waiters.current.push((tok) => {
        ready.current = tok;
        resolve(take()!);
      }));
    },
  }), []);

  if (enabled === false) return null;
  return <div className="captcha" ref={box} />;
});

export default Turnstile;
