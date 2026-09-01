// gcf_more_test.go - coverage for gcf.go: env parsing of gcfEnabled (t.Setenv save/restores automatically), and
// the two structuredResult branches (GCF-on emits a text block with no StructuredContent; forced-JSON returns
// the payload as StructuredContent). No plugin.

package sidechain

import (
	"strings"
	"testing"
)

func TestGCFEnabled(t *testing.T) {
	off := []string{"json", "0", "off", "false", "JSON", "Off"}
	for _, v := range off {
		t.Setenv("SIDECHAIN_MCP_FORMAT", v)
		if gcfEnabled() {
			t.Errorf("SIDECHAIN_MCP_FORMAT=%q should disable GCF", v)
		}
	}
	on := []string{"", "gcf", "yes", "1", "anything"}
	for _, v := range on {
		t.Setenv("SIDECHAIN_MCP_FORMAT", v)
		if !gcfEnabled() {
			t.Errorf("SIDECHAIN_MCP_FORMAT=%q should keep GCF on (default)", v)
		}
	}
}

func TestStructuredResultBranches(t *testing.T) {
	payload := struct {
		Count  int      `json:"count"`
		Groups []string `json:"groups"`
	}{Count: 2, Groups: []string{"filter", "amp"}}

	// GCF on: text carries the summary plus an encoded block, and there is NO StructuredContent.
	t.Setenv("SIDECHAIN_MCP_FORMAT", "gcf")
	res, structured, err := structuredResult("summary line", payload)
	if err != nil {
		t.Fatalf("structuredResult(gcf): %v", err)
	}
	if structured != nil {
		t.Fatalf("GCF path should return nil StructuredContent, got %+v", structured)
	}
	if !strings.Contains(textOf(res), "summary line") {
		t.Fatalf("GCF text missing summary: %q", textOf(res))
	}

	// Forced JSON: text is just the summary and the payload comes back as StructuredContent.
	t.Setenv("SIDECHAIN_MCP_FORMAT", "json")
	res2, structured2, err := structuredResult("summary line", payload)
	if err != nil {
		t.Fatalf("structuredResult(json): %v", err)
	}
	if structured2 == nil {
		t.Fatal("JSON path should return the payload as StructuredContent")
	}
	if textOf(res2) != "summary line" {
		t.Fatalf("JSON text = %q, want bare summary", textOf(res2))
	}
}
