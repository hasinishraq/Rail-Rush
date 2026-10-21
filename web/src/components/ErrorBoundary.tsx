import { Component, type ErrorInfo, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { t } from "../i18n";

/**
 * Catches render errors so a crash in one page shows a message instead of a
 * blank screen. App keys it on the location, so navigating away resets it.
 */
export default class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("Page crashed:", error, info.componentStack);
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    return (
      <div className="empty">
        <p>{t("eb.text")} <button className="linkish" onClick={() => location.reload()}>{t("eb.reload")}</button> {t("eb.or")} <Link to="/">{t("eb.search")}</Link>.</p>
        <p className="error">{error.message}</p>
      </div>
    );
  }
}
