import { useState } from "react";
import { Link, NavLink } from "react-router-dom";
import { useSettings } from "../lib/settings";
import { useTheme } from "../lib/theme";

const THEME_ICON = { system: "🖥️", light: "☀️", dark: "🌙" };
const THEME_LABEL = { system: "Theme: system", light: "Theme: light", dark: "Theme: dark" };

export function Header() {
  const { settings, setSettings, isConfigured } = useSettings();
  const { theme, cycleTheme } = useTheme();
  const [open, setOpen] = useState(!isConfigured);
  const [draft, setDraft] = useState(settings);

  const save = () => {
    setSettings(draft);
    setOpen(false);
  };

  return (
    <header className="app-header">
      <div className="app-header-row">
        <Link to="/" className="brand">
          nexus web
        </Link>
        <nav className="nav-links">
          <NavLink to="/approvals">Approvals</NavLink>
        </nav>
        <div className="header-spacer" />
        <button type="button" className="theme-toggle" title={THEME_LABEL[theme]} onClick={cycleTheme}>
          {THEME_ICON[theme]}
        </button>
        <button
          type="button"
          className={`settings-toggle ${isConfigured ? "" : "warn"}`}
          onClick={() => {
            setDraft(settings);
            setOpen((o) => !o);
          }}
        >
          {isConfigured ? "Settings" : "Set up identity"}
        </button>
      </div>

      {open && (
        <div className="settings-panel">
          <p className="settings-note">
            Paste a bearer token minted with <code>nexusd token --tenant=&lt;name&gt;</code> (add{" "}
            <code>--user=&lt;uuid&gt;</code> to pin a specific user). The backend verifies this
            token and derives your tenant/user identity from its claims — it never trusts
            anything else the client sends.
          </p>
          <div className="settings-fields">
            <label>
              Base URL
              <input
                type="text"
                value={draft.baseUrl}
                placeholder="http://localhost:8085"
                onChange={(e) => setDraft({ ...draft, baseUrl: e.target.value })}
              />
            </label>
            <label>
              Bearer token
              <input
                type="password"
                value={draft.token}
                placeholder="eyJhbGciOi..."
                onChange={(e) => setDraft({ ...draft, token: e.target.value })}
              />
            </label>
          </div>
          <div className="settings-actions">
            <button type="button" onClick={save}>
              Save
            </button>
            <button type="button" className="secondary" onClick={() => setOpen(false)}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </header>
  );
}
