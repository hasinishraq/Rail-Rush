import { useEffect, useState } from "react";
import { t, type Key } from "../i18n";

/** Three text sizes; the whole page is sized in rem, so it all scales. */
const SIZES: { key: string; label: Key; scale: string; glyph: string }[] = [
  { key: "normal", label: "ts.normal", scale: "100%", glyph: "A" },
  { key: "large", label: "ts.large", scale: "112.5%", glyph: "A" },
  { key: "larger", label: "ts.larger", scale: "125%", glyph: "A" },
];
const KEY = "rail.textsize";

function stored(): string {
  try {
    return localStorage.getItem(KEY) ?? "normal";
  } catch {
    return "normal";
  }
}

export default function TextSize() {
  const [size, setSize] = useState(stored);

  useEffect(() => {
    const s = SIZES.find((x) => x.key === size) ?? SIZES[0];
    document.documentElement.style.fontSize = s.scale;
    try {
      localStorage.setItem(KEY, s.key);
    } catch {
      /* private mode: the size just isn't remembered */
    }
  }, [size]);

  return (
    <div className="textsize" role="group" aria-label={t("ts.group")}>
      {SIZES.map((s, i) => (
        <button key={s.key} type="button" className={`ts-${i}`} aria-pressed={size === s.key} aria-label={t(s.label)}
          title={t(s.label)} onClick={() => setSize(s.key)}>
          {s.glyph}
        </button>
      ))}
    </div>
  );
}
