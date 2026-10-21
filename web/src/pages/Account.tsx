import { useEffect, useState, type FormEvent } from "react";
import { api, ApiError, idemKey, type Me, type Page, type PassengerIn, type SavedPassenger } from "../api";
import { t, tCode } from "../i18n";

const emptyForm = (): PassengerIn => ({ name: "", gender: "", type: "ADULT" });

export default function Account() {
  const [me, setMe] = useState<Me | null>(null);
  const [list, setList] = useState<SavedPassenger[]>([]);
  const [form, setForm] = useState<PassengerIn>(emptyForm());
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const reload = () => api<Page<SavedPassenger>>("GET", "/v1/me/passengers").then((p) => setList(p.items));
  useEffect(() => {
    api<Me>("GET", "/v1/me").then(setMe).catch((e) => setError((e as Error).message));
    reload().catch(() => undefined);
  }, []);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api("POST", "/v1/me/passengers", { ...form, name: form.name.trim() }, { headers: { "Idempotency-Key": idemKey() } });
      setForm(emptyForm());
      await reload();
    } catch (err) {
      const a = err as ApiError;
      setError(Object.values((a.details.fields as Record<string, string>) ?? {}).join(". ") || a.message);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: number) => {
    await api("DELETE", `/v1/me/passengers/${id}`).catch((e) => setError((e as Error).message));
    await reload();
  };

  return (
    <div className="narrow">
      <h1>{t("ac.title")}</h1>
      {me && (
        <dl className="facts">
          <div><dt>{t("f.name")}</dt><dd>{me.name}</dd></div>
          {me.phone && <div><dt>{t("ac.mobile")}</dt><dd>{me.phone}</dd></div>}
          {me.email && <div><dt>{t("ac.email")}</dt><dd>{me.email}</dd></div>}
          <div><dt>{t("ac.identity")}</dt><dd>{me.verified ? t("ac.verified") : t("ac.notVerified")}</dd></div>
        </dl>
      )}

      <h2>{t("ac.saved")}</h2>
      <p className="lede">{t("ac.savedLede")}</p>
      {list.length === 0 ? (
        <p className="muted">{t("ac.noneSaved")}</p>
      ) : (
        <ul className="saved-list">
          {list.map((p) => (
            <li key={p.id}>
              <span>{p.name}, {tCode("f", p.gender.toLowerCase())}{p.type === "CHILD" ? `, ${t("ac.childTag")}` : ""}</span>
              <button className="linkish" onClick={() => remove(p.id)}>{t("ac.remove", { name: p.name })}</button>
            </li>
          ))}
        </ul>
      )}
      {list.length < 20 && (
        <form className="form saved-form" onSubmit={add}>
          <label className="field">
            <span>{t("f.name")}</span>
            <input required maxLength={40} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </label>
          <div className="pax-row">
            <label className="field">
              <span>{t("f.gender")}</span>
              <select required value={form.gender} onChange={(e) => setForm({ ...form, gender: e.target.value })}>
                <option value="">{t("f.choose")}</option>
                <option value="FEMALE">{t("f.female")}</option>
                <option value="MALE">{t("f.male")}</option>
                <option value="OTHER">{t("f.other")}</option>
              </select>
            </label>
            <label className="field">
              <span>{t("f.ticket")}</span>
              <select value={form.type} onChange={(e) => setForm({ ...form, type: e.target.value })}>
                <option value="ADULT">{t("f.adult")}</option>
                <option value="CHILD">{t("f.child")}</option>
              </select>
            </label>
          </div>
          {error && <p className="error" role="alert">{error}</p>}
          <button className="btn btn-primary" disabled={busy}>{t("ac.save")}</button>
        </form>
      )}
    </div>
  );
}
