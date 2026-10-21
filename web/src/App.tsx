import { Link, NavLink, Navigate, Route, Routes, useLocation } from "react-router-dom";
import { useEffect, useState, type ReactNode } from "react";
import { logout } from "./api";
import { useUser } from "./hooks";
import { lang, setLang, t, useLang } from "./i18n";
import Search from "./pages/Search";
import { Login, Register } from "./pages/Auth";
import Journey from "./pages/Journey";
import Booking from "./pages/Booking";
import PayReturn from "./pages/PayReturn";
import Trips from "./pages/Trips";
import Account from "./pages/Account";
import ErrorBoundary from "./components/ErrorBoundary";
import TextSize from "./components/TextSize";
import { loadStations } from "./stations";

function RequireUser({ children }: { children: ReactNode }) {
  const user = useUser();
  const loc = useLocation();
  if (!user) return <Navigate to={`/login?next=${encodeURIComponent(loc.pathname + loc.search)}`} replace />;
  return <>{children}</>;
}

export default function App() {
  const user = useUser();
  const loc = useLocation();
  useLang(); // re-render everything in the new language when it changes
  // Bangla station names for pages that only get codes; re-render once they arrive.
  const [, setStationsReady] = useState(false);
  useEffect(() => {
    loadStations().then(() => setStationsReady(true));
  }, []);
  return (
    <>
      <a className="skip" href="#main">{t("app.skip")}</a>
      <header className="topbar">
        <div className="topbar-inner">
          <Link to="/" className="brand" aria-label={t("app.home")}>
            <span className="brand-mark" aria-hidden="true" />
            Rail<span className="brand-accent">Rush</span>
          </Link>
          <nav className="nav" aria-label={t("nav.main")}>
            <NavLink to="/" end>{t("nav.book")}</NavLink>
            {user && <NavLink to="/trips">{t("nav.trips")}</NavLink>}
            {user ? (
              <>
                <NavLink to="/account" title={user.name}>{t("nav.account")}</NavLink>
                <button className="linkish" onClick={() => logout()}>{t("nav.signOut")}</button>
              </>
            ) : (
              <NavLink to="/login">{t("nav.signIn")}</NavLink>
            )}
          </nav>
          <div className="topbar-tools">
            <button type="button" className="lang-switch" lang={lang() === "bn" ? "en" : "bn"} title={t("lang.switch")}
              aria-label={t("lang.switch")} onClick={() => setLang(lang() === "bn" ? "en" : "bn")}>
              {t("lang.other")}
            </button>
            <TextSize />
          </div>
        </div>
        <div className="livery-band" aria-hidden="true" />
      </header>
      <main id="main" className="page">
        <ErrorBoundary key={loc.pathname}>
        <Routes>
          <Route path="/" element={<Search />} />
          <Route path="/login" element={<Login />} />
          <Route path="/register" element={<Register />} />
          <Route path="/journey/:id" element={<RequireUser><Journey /></RequireUser>} />
          <Route path="/bookings/:pnr" element={<RequireUser><Booking /></RequireUser>} />
          <Route path="/pay/return" element={<RequireUser><PayReturn /></RequireUser>} />
          <Route path="/trips" element={<RequireUser><Trips /></RequireUser>} />
          <Route path="/account" element={<RequireUser><Account /></RequireUser>} />
          <Route path="*" element={<p className="empty">{t("app.notFound")} <Link to="/">{t("app.searchTrains")}</Link></p>} />
        </Routes>
        </ErrorBoundary>
      </main>
      <footer className="footer">
        <div className="footer-inner">
          <span><b>RailRush</b>, {t("footer.tag")}</span>
          <span>{t("footer.note")}</span>
        </div>
      </footer>
    </>
  );
}
