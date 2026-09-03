import { findTaint } from "../lib/taint";

/** Renders a small row of pills for a Rule-of-Two taint shape, if one is
 * present in the given event body. Renders nothing otherwise. */
export function TaintBadges({ body }: { body: unknown }) {
  const legs = findTaint(body);
  if (!legs) return null;

  return (
    <div className="taint-badges" title="Rule-of-Two taint state">
      {legs.map((leg) => (
        <span key={leg.label} className={`taint-pill ${leg.engaged ? "engaged" : ""}`}>
          {leg.label}
        </span>
      ))}
    </div>
  );
}
