import { useState } from "react";
import { Link, NavLink } from "react-router-dom";
import { useSettings } from "../lib/settings";

export function Header() {
  const { settings, setSettings, isConfigured } = useSettings();
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
          <NavLink to="/" end>
            New run
          </NavLink>
          <NavLink to="/approvals">Approvals</NavLink>
        </nav>
        <div className="header-spacer" />
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
            Dev auth: this backend has no real login yet — it trusts whatever tenant/user UUID
            you paste here, read fresh from headers on every request. Paste any tenant/user
            UUID (e.g. from <code>make seed</code> output or your own test fixtures).
          </p>
          <div className="settings-fields">
            <label>
              Base URL
              <input
                type="text"
                value={draft.baseUrl}
                placeholder="http://localhost:8080"
                onChange={(e) => setDraft({ ...draft, baseUrl: e.target.value })}
              />
            </label>
            <label>
              Tenant ID
              <input
                type="text"
                value={draft.tenantId}
                placeholder="00000000-0000-0000-0000-000000000000"
                onChange={(e) => setDraft({ ...draft, tenantId: e.target.value })}
              />
            </label>
            <label>
              User ID
              <input
                type="text"
                value={draft.userId}
                placeholder="00000000-0000-0000-0000-000000000000"
                onChange={(e) => setDraft({ ...draft, userId: e.target.value })}
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
