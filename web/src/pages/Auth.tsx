import { useRef, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { ApiError, login, register } from "../api";
import { t } from "../i18n";
import Turnstile, { type CaptchaHandle } from "../components/Turnstile";

/** Where to go after signing in: only a path on this site, never "//host" or "/\host" (open redirect). */
function safeNext(raw: string | null): string {
  return raw && raw.startsWith("/") && !raw.startsWith("//") && !raw.startsWith("/\\") ? raw : "/";
}

export function Login() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [id, setId] = useState(params.get("login") ?? "");
  const [pw, setPw] = useState("");
  const captcha = useRef<CaptchaHandle>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const next = safeNext(params.get("next"));

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await login(id, pw, await captcha.current?.token());
      navigate(next, { replace: true });
    } catch (err) {
      const a = err as ApiError;
      if (a.code === "INVALID_CREDENTIALS") setError(t("au.wrong"));
      else if (a.code === "CAPTCHA_INVALID") setError(t("au.captchaFailed"));
      else if (a.code === "RATE_LIMITED") setError(t("au.tooMany"));
      else setError(a.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="narrow form" onSubmit={submit}>
      <h1>{t("au.signIn")}</h1>
      <p className="lede">{t("au.lede")}</p>
      <label className="field">
        <span>{t("au.id")}</span>
        <input autoComplete="username" required value={id} onChange={(e) => setId(e.target.value)} placeholder={t("au.idPh")} />
      </label>
      <label className="field">
        <span>{t("au.password")}</span>
        <input type="password" autoComplete="current-password" required value={pw} onChange={(e) => setPw(e.target.value)} />
      </label>
      <Turnstile ref={captcha} />
      {error && <p className="error" role="alert">{error}</p>}
      <button className="btn btn-primary" disabled={busy}>{busy ? t("au.signingIn") : t("au.signIn")}</button>
      <p className="muted">{t("au.new")} <Link to={`/register?next=${encodeURIComponent(next)}`}>{t("au.create")}</Link></p>
    </form>
  );
}

export function Register() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const [f, setF] = useState({ name: "", phone: "+8801", email: "", nid: "", password: "" });
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const captcha = useRef<CaptchaHandle>(null);
  const set = (k: keyof typeof f) => (e: React.ChangeEvent<HTMLInputElement>) => setF({ ...f, [k]: e.target.value });

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErrors({});
    setError("");
    try {
      await register({ ...f, email: f.email || undefined, nid: f.nid || undefined }, await captcha.current?.token());
      await login(f.phone, f.password, await captcha.current?.token());
      navigate(safeNext(params.get("next")), { replace: true });
    } catch (err) {
      const a = err as ApiError;
      const fields = (a.details?.fields ?? {}) as Record<string, string>;
      if (fields.phone === "taken") fields.phone = t("au.phoneTaken");
      if (fields.email === "taken") fields.email = t("au.emailTaken");
      if (fields.nid === "taken") fields.nid = t("au.nidTaken");
      setErrors(fields);
      if (a.code === "CAPTCHA_INVALID") setError(t("au.captchaFailed"));
      else if (!Object.keys(fields).length) setError(a.message);
    } finally {
      setBusy(false);
    }
  };

  const fieldErr = (k: string) => errors[k] && <span className="field-error">{errors[k]}</span>;

  return (
    <form className="narrow form" onSubmit={submit}>
      <h1>{t("au.create")}</h1>
      <label className="field">
        <span>{t("au.fullName")}</span>
        <input autoComplete="name" required value={f.name} onChange={set("name")} />
        {fieldErr("name")}
      </label>
      <label className="field">
        <span>{t("au.mobile")}</span>
        <input type="tel" autoComplete="tel" required pattern="\+8801[3-9][0-9]{8}" placeholder="+8801XXXXXXXXX" value={f.phone} onChange={set("phone")} />
        {fieldErr("phone")}
      </label>
      <label className="field">
        <span>{t("au.emailOpt")}</span>
        <input type="email" autoComplete="email" value={f.email} onChange={set("email")} />
        {fieldErr("email")}
      </label>
      <label className="field">
        <span>{t("au.nid")}</span>
        <input inputMode="numeric" value={f.nid} onChange={set("nid")} />
        {fieldErr("nid")}
      </label>
      <label className="field">
        <span>{t("au.pw10")}</span>
        <input type="password" autoComplete="new-password" required minLength={10} value={f.password} onChange={set("password")} />
        {fieldErr("password")}
      </label>
      <Turnstile ref={captcha} />
      {error && <p className="error" role="alert">{error}</p>}
      <button className="btn btn-primary" disabled={busy}>{busy ? t("au.creating") : t("au.createBtn")}</button>
      <p className="muted">{t("au.already")} <Link to="/login">{t("au.signIn")}</Link></p>
    </form>
  );
}
