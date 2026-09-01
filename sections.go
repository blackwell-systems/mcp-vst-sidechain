// sections.go - label-prefix sectioning: a navigation fallback for plugins that report NO parameter groups.
//
// Many plugins expose a flat parameter list (every param lands in "other"), yet their labels carry clear
// structure: "Filter Cutoff", "Filter Resonance", "Amp Attack", "Osc 1 Tune", "LFO 1 Rate". This file derives
// navigable SECTIONS from those shared label prefixes so an agent can page a flat plugin by section
// (list_params group=Filter) instead of scanning hundreds of entries.
//
// Derivation is a catalog-level VIEW only: it never mutates ParamDef.Group (the wire shape is untouched). It is
// applied as a FALLBACK - only when the catalog is effectively ungrouped (every param is "other"/empty). When a
// plugin exposes real groups, those are kept verbatim and no derivation happens.
//
// The C++ host mirrors this algorithm in JucePluginBridge::deriveLabelSections (cpp/JucePluginBridge.h) to bind
// the C3 governed layer's leasable SECTIONS on a flat plugin, so an agent paging a flat plugin by section here can
// lease that same section on the host. Keep the two in lockstep (verified against TAL-NoiseMaker's real labels).

package sidechain

import (
	"sort"
	"strings"
)

// sectionMinShare is the minimum number of params that must share a leading label-prefix before that prefix
// becomes its own derived section. Prefixes shared by fewer params are not promoted; those params fall to
// "other". Named so the threshold is easy to tune.
const sectionMinShare = 3

// derivedOther is the catch-all section for params with no qualifying shared prefix. It sorts last.
const derivedOther = "other"

// labelTokens splits a label into its ordered, section-relevant tokens: split on any non-alphanumeric run, then
// drop pure-number tokens so "Osc 1 Tune" and "Osc 2 Tune" share the leading token "Osc". Original casing is
// preserved (section names read like the labels). Returns nil for a label with no usable tokens.
func labelTokens(label string) []string {
	fields := strings.FieldsFunc(label, func(r rune) bool {
		return !isAlphaNum(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if isAllDigits(f) {
			continue // "1", "2" are indices, not section words
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isAlphaNum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// deriveSections computes a section name for each param from label prefixes. Returns a slice parallel to params:
// out[i] is the section for params[i]. A leading token-prefix becomes a section when it is shared (as a prefix)
// by at least sectionMinShare params; each param is assigned to the LONGEST such qualifying prefix. Params with
// no qualifying shared prefix get derivedOther. The mapping is deterministic (it depends only on the labels).
func deriveSections(params []ParamDef) []string {
	// tokens[i] is the section-relevant token sequence for params[i].
	tokens := make([][]string, len(params))
	// count how many params share each candidate prefix (joined by a single space, casing preserved).
	prefixCount := map[string]int{}
	for i := range params {
		tk := labelTokens(params[i].Label)
		tokens[i] = tk
		for l := 1; l <= len(tk); l++ {
			prefixCount[strings.Join(tk[:l], " ")]++
		}
	}

	out := make([]string, len(params))
	for i := range params {
		tk := tokens[i]
		section := derivedOther
		// longest qualifying prefix wins: walk from the full length down to 1.
		for l := len(tk); l >= 1; l-- {
			p := strings.Join(tk[:l], " ")
			if prefixCount[p] >= sectionMinShare {
				section = p
				break
			}
		}
		out[i] = section
	}
	return out
}

// sortedSections returns the distinct section names in a stable order: alphabetical (case-insensitive), with
// derivedOther always last.
func sortedSections(sections []string) []string {
	seen := map[string]bool{}
	var names []string
	for _, s := range sections {
		if !seen[s] {
			seen[s] = true
			names = append(names, s)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == derivedOther {
			return false
		}
		if names[j] == derivedOther {
			return true
		}
		li, lj := strings.ToLower(names[i]), strings.ToLower(names[j])
		if li != lj {
			return li < lj
		}
		return names[i] < names[j]
	})
	return names
}
