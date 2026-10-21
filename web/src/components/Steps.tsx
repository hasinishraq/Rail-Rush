import { num, t, type Key } from "../i18n";

/**
 * Where the traveller is in the booking, drawn as a railway line with one
 * station per step: passed stations are filled, the current one is ringed.
 */
const STEPS: Key[] = ["step.0", "step.1", "step.2", "step.3", "step.4"];

export default function Steps({ current }: { current: number }) {
  return (
    <nav className="steps" aria-label={t("steps.label")}>
      <ol>
        {STEPS.map((s, i) => {
          const state = i < current ? "done" : i === current ? "now" : "next";
          return (
            <li key={s} className={`step step-${state}`} aria-current={state === "now" ? "step" : undefined}>
              <span className="step-stop" aria-hidden="true">{state === "done" ? "✓" : num(i + 1)}</span>
              <span className="step-name">
                {t(s)}
                {state === "done" && <span className="sr-only"> {t("steps.done")}</span>}
              </span>
            </li>
          );
        })}
      </ol>
      <p className="steps-caption" aria-hidden="true">{t("steps.caption", { n: current + 1, total: STEPS.length, name: t(STEPS[current]) })}</p>
    </nav>
  );
}
