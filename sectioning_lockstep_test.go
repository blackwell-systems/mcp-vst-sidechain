// sectioning_lockstep_test.go - a gated cross-check that the HOST's emitted per-param `section` (computed by
// cpp/Sectioning.h) equals the Go reference derivation (sections.go) on a real plugin's catalog. Sectioning now
// lives in the host (the Go catalog prefers the host-emitted `section`), but the Go derivation is retained as a
// fallback and as the reference oracle; this test makes the two implementations' equivalence machine-checked in
// CI, per plugin, instead of a manual comparison. Gated on SIDECHAIN_SWEEP_CATALOG; run by drive_plugin.sh for
// every hosted plugin (Surge grouped, TAL flat/derived, Dexed, ...).

package sidechain

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestSectionLockstep(t *testing.T) {
	catPath := os.Getenv("SIDECHAIN_SWEEP_CATALOG")
	if catPath == "" {
		t.Skip("set SIDECHAIN_SWEEP_CATALOG to cross-check host-emitted sections against the Go derivation")
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	// Parse raw params (not via NewCatalog, so we can compare the host's Section against the Go reference directly).
	var raw struct {
		Params []ParamDef `json:"params"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	params := raw.Params
	if len(params) == 0 {
		t.Fatal("catalog has no params")
	}

	// The host must emit a section for every param.
	for i := range params {
		if strings.TrimSpace(params[i].Section) == "" {
			t.Fatalf("param %s (%q) has no host-emitted section", params[i].ID, params[i].Label)
		}
	}

	// Go reference effective section: raw groups verbatim when the plugin is grouped, else label-prefix derivation.
	hasReal := false
	for i := range params {
		if g := params[i].Group; g != "" && g != "other" {
			hasReal = true
			break
		}
	}
	ref := make([]string, len(params))
	if hasReal {
		for i := range params {
			g := strings.TrimSpace(params[i].Group)
			if g == "" {
				g = "other"
			}
			ref[i] = g
		}
	} else {
		ref = deriveSections(params)
	}

	mismatch := 0
	for i := range params {
		if params[i].Section != ref[i] {
			if mismatch < 5 {
				t.Errorf("section mismatch for %s (%q): host=%q go=%q", params[i].ID, params[i].Label, params[i].Section, ref[i])
			}
			mismatch++
		}
	}
	if mismatch != 0 {
		t.Fatalf("%d/%d params where the host section differs from the Go reference (lockstep broken)", mismatch, len(params))
	}
	t.Logf("section lockstep OK: host == Go reference for all %d params (grouped=%v)", len(params), hasReal)
}
