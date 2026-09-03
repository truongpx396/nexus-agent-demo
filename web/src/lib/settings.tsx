import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import type { Settings } from "./types";

const STORAGE_KEY = "nexus-web-settings";

const DEFAULT_SETTINGS: Settings = {
  baseUrl: "http://localhost:8080",
  tenantId: "",
  userId: "",
};

function loadSettings(): Settings {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return DEFAULT_SETTINGS;
    const parsed = JSON.parse(raw);
    return { ...DEFAULT_SETTINGS, ...parsed };
  } catch {
    return DEFAULT_SETTINGS;
  }
}

interface SettingsContextValue {
  settings: Settings;
  setSettings: (next: Settings) => void;
  isConfigured: boolean;
}

const SettingsContext = createContext<SettingsContextValue | null>(null);

export function SettingsProvider({ children }: { children: ReactNode }) {
  const [settings, setSettingsState] = useState<Settings>(() => loadSettings());

  useEffect(() => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(settings));
  }, [settings]);

  const setSettings = (next: Settings) => setSettingsState(next);

  const isConfigured = useMemo(
    () => Boolean(settings.baseUrl && settings.tenantId && settings.userId),
    [settings],
  );

  return (
    <SettingsContext.Provider value={{ settings, setSettings, isConfigured }}>
      {children}
    </SettingsContext.Provider>
  );
}

export function useSettings(): SettingsContextValue {
  const ctx = useContext(SettingsContext);
  if (!ctx) throw new Error("useSettings must be used within a SettingsProvider");
  return ctx;
}
