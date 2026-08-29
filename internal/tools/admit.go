package tools

import (
	"regexp"
	"strings"
)

// AdmissionStatus is a descriptor's scan verdict (README task 3.3, pattern
// 15): pending until scanned, then clean, flagged, or rejected. The
// pipeline's admission gate (step 3) refuses dispatch on anything but
// clean, fail closed.
type AdmissionStatus string

const (
	AdmissionPending  AdmissionStatus = "pending"
	AdmissionClean    AdmissionStatus = "clean"
	AdmissionFlagged  AdmissionStatus = "flagged"
	AdmissionRejected AdmissionStatus = "rejected"
)

// injectionPatterns are phrases that have no legitimate reason to appear in
// a tool's own description or schema — a descriptor is catalog metadata the
// model reads on every turn (the byte-stable prefix), so a descriptor that
// tries to talk the model into something is itself an injection vector, not
// just tool output. This is demo-scoped illustrative policy: real coverage
// belongs in a maintained corpus (evals/corpus/safety/), not a fixed list.
var injectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all )?(previous|prior|above) instructions`),
	regexp.MustCompile(`(?i)disregard (the )?system prompt`),
	regexp.MustCompile(`(?i)you must (always|now) `),
	regexp.MustCompile(`(?i)<!--.*-->`), // an HTML comment hiding text from a rendered UI but not from the model
}

// suspiciousPatterns are weaker signals: not injection on their own, but
// worth a human's attention before a descriptor is trusted.
var suspiciousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[\x{200B}-\x{200F}\x{2060}-\x{206F}]`), // zero-width / bidi-control characters
}

// Finding is one scan hit.
type Finding struct {
	Pattern string
	Field   string // "description" or "input_schema"
}

// Scan is the admission scanner (task 3.3): rejected on an injection-grade
// match, flagged on a weaker signal, clean otherwise. A descriptor that
// fails to scan at all (this function never errors, by construction — an
// empty descriptor scans clean) still goes through the same fail-closed
// default the caller applies to an unscanned (AdmissionPending) tool.
func Scan(d Descriptor) (AdmissionStatus, []Finding) {
	var findings []Finding

	check := func(field, text string) {
		for _, p := range injectionPatterns {
			if p.MatchString(text) {
				findings = append(findings, Finding{Pattern: p.String(), Field: field})
			}
		}
	}
	checkSuspicious := func(field, text string) {
		for _, p := range suspiciousPatterns {
			if p.MatchString(text) {
				findings = append(findings, Finding{Pattern: p.String(), Field: field})
			}
		}
	}

	check("description", d.Description)
	check("input_schema", string(d.InputSchema))

	if len(findings) > 0 {
		return AdmissionRejected, findings
	}

	checkSuspicious("description", d.Description)
	checkSuspicious("input_schema", string(d.InputSchema))
	if len(findings) > 0 {
		return AdmissionFlagged, findings
	}

	if strings.TrimSpace(d.Description) == "" {
		return AdmissionFlagged, []Finding{{Pattern: "empty description", Field: "description"}}
	}

	return AdmissionClean, nil
}
