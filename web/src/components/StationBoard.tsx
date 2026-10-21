import { useEffect, useId, useRef, useState } from "react";
import { publicGet, type Station } from "../api";
import { lang, t } from "../i18n";

/** The station name in the reader's language, and the other one as a second line. */
export const localName = (s: Station) => (lang() === "bn" && s.name_bn ? s.name_bn : s.name);
const otherName = (s: Station) => (lang() === "bn" ? s.name : s.name_bn);

interface Props {
  label: string;
  value: Station | null;
  onChange: (s: Station | null) => void;
}

/** A yellow platform name board that turns into a station search field. */
export default function StationBoard({ label, value, onChange }: Props) {
  const [q, setQ] = useState("");
  const [open, setOpen] = useState(false);
  const [items, setItems] = useState<Station[]>([]);
  const [active, setActive] = useState(0);
  const listId = useId();
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!open) return;
    const ctl = new AbortController();
    const t = setTimeout(() => {
      publicGet<{ items: Station[] }>(`/v1/stations?limit=8&q=${encodeURIComponent(q)}`, ctl.signal)
        .then((d) => {
          setItems(d?.items ?? []);
          setActive(0);
        })
        .catch(() => undefined);
    }, 120);
    return () => {
      clearTimeout(t);
      ctl.abort();
    };
  }, [q, open]);

  const pick = (s: Station) => {
    onChange(s);
    setOpen(false);
    setQ("");
  };

  return (
    <div className="board-wrap">
      <span className="board-label">{label}</span>
      {open ? (
        <div className="board board-editing">
          <input
            ref={inputRef}
            autoFocus
            role="combobox"
            aria-expanded="true"
            aria-controls={listId}
            aria-label={t("station.input", { label })}
            className="board-input"
            placeholder={t("station.placeholder")}
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onBlur={() => setTimeout(() => setOpen(false), 150)}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown") setActive((a) => Math.min(a + 1, items.length - 1));
              else if (e.key === "ArrowUp") setActive((a) => Math.max(a - 1, 0));
              else if (e.key === "Enter" && items[active]) {
                e.preventDefault();
                pick(items[active]);
              } else if (e.key === "Escape") setOpen(false);
            }}
          />
          <ul className="board-list" id={listId} role="listbox">
            {items.map((s, i) => (
              <li
                key={s.code}
                role="option"
                aria-selected={i === active}
                className={i === active ? "active" : ""}
                onMouseDown={(e) => {
                  e.preventDefault();
                  pick(s);
                }}
              >
                <span className="bl-name">{localName(s)}</span>
                {otherName(s) && <span className="bl-bn">{otherName(s)}</span>}
                <span className="bl-code">{s.code}</span>
              </li>
            ))}
            {items.length === 0 && <li className="muted">{t("station.none", { q })}</li>}
          </ul>
        </div>
      ) : (
        <button type="button" className="board" onClick={() => setOpen(true)} aria-label={t("station.aria", { label, value: value ? localName(value) : t("station.choose") })}>
          {value ? (
            <>
              <span className="board-name">{localName(value)}</span>
              <span className="board-sub">
                {otherName(value) && <span>{otherName(value)}</span>}
                <span className="board-code">{value.code}</span>
              </span>
            </>
          ) : (
            <>
              <span className="board-name board-empty">{t("station.empty")}</span>
              <span className="board-sub">{t("station.emptySub")}</span>
            </>
          )}
        </button>
      )}
    </div>
  );
}
