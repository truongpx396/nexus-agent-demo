import { useState } from "react";

const COLLAPSE_THRESHOLD = 400; // characters

/** Pretty-printed JSON, collapsed behind a toggle once it gets long. */
export function JsonView({ value, defaultOpen }: { value: unknown; defaultOpen?: boolean }) {
  const text = formatValue(value);
  const long = text.length > COLLAPSE_THRESHOLD;
  const [open, setOpen] = useState(defaultOpen ?? !long);

  if (!long) {
    return <pre className="json-view">{text}</pre>;
  }

  return (
    <div className="json-view-collapsible">
      <button type="button" className="json-toggle" onClick={() => setOpen((o) => !o)}>
        {open ? "collapse" : "expand"} ({text.length} chars)
      </button>
      {open && <pre className="json-view">{text}</pre>}
    </div>
  );
}

function formatValue(value: unknown): string {
  if (value === undefined) return "";
  if (typeof value === "string") {
    // body/context sometimes arrive already-stringified JSON; try to
    // re-parse so it pretty-prints instead of showing as one long string.
    try {
      return JSON.stringify(JSON.parse(value), null, 2);
    } catch {
      return value;
    }
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}
