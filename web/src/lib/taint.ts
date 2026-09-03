// Best-effort detection of a Rule-of-Two taint shape inside an event body.
// This is deliberately conservative: it only recognizes a few known
// spellings and bails out (returns null) rather than guessing at a shape
// that isn't there. Skip rendering, don't fabricate.

export interface TaintLeg {
  label: string;
  engaged: boolean;
}

const NAMED_LEGS: Array<{ keys: string[]; label: string }> = [
  { keys: ["untrusted_input", "untrustedInput"], label: "untrusted input" },
  { keys: ["private_data", "privateData"], label: "private data" },
  { keys: ["external_effect", "externalEffect"], label: "external effect" },
];

const CONTAINER_KEYS = ["taint", "taint_state", "taintState", "rule_of_two", "ruleOfTwo"];

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function fromNamedLegs(obj: Record<string, unknown>): TaintLeg[] | null {
  const legs: TaintLeg[] = [];
  for (const { keys, label } of NAMED_LEGS) {
    const key = keys.find((k) => typeof obj[k] === "boolean");
    if (!key) return null;
    legs.push({ label, engaged: obj[key] as boolean });
  }
  return legs;
}

function fromBooleanTriple(arr: unknown[]): TaintLeg[] | null {
  if (arr.length !== 3 || !arr.every((v) => typeof v === "boolean")) return null;
  return arr.map((v, i) => ({ label: `leg ${i + 1}`, engaged: v as boolean }));
}

/** Looks at the top level of a body, plus one level under a taint-ish key. */
export function findTaint(body: unknown): TaintLeg[] | null {
  if (!isPlainObject(body)) return null;

  const direct = fromNamedLegs(body);
  if (direct) return direct;

  for (const key of CONTAINER_KEYS) {
    const candidate = body[key];
    if (Array.isArray(candidate)) {
      const legs = fromBooleanTriple(candidate);
      if (legs) return legs;
    } else if (isPlainObject(candidate)) {
      const legs = fromNamedLegs(candidate);
      if (legs) return legs;
    }
  }

  return null;
}
