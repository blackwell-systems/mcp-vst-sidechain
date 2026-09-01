// au_live_test.go - AU (.component / AudioUnit) smoke test. macOS-only and AU-specific: it proves the C++ host
// can load an AudioUnit (by component identifier, e.g. "AudioUnit:Effects/aufx,dcmp,appl"), enumerate its
// parameter catalog, and be driven over the control socket exactly like a VST3. Everything else in the suite has
// only ever exercised VST3; this closes the AU gap. Gated on env (SIDECHAIN_AU_PORT/CATALOG/PARAM), driven by
// the integration workflow against an Apple built-in AU; skipped otherwise so the normal suite stays green.
//
// Why an Apple built-in AU and not a third-party .component: on macOS an AU is resolved through the system
// AudioComponent registry (AudioComponentFindNext), not by scanning an arbitrary path. A third-party .component
// only loads once it is installed under ~/Library/Audio/Plug-Ins/Components and registered by
// AudioComponentRegistrar, which is unreliable on a headless CI runner. Apple's own units are always registered,
// so referencing one by identifier is the deterministic way to smoke-test the AU load+drive path.

package sidechain

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestAULive(t *testing.T) {
	port := os.Getenv("SIDECHAIN_AU_PORT")
	catPath := os.Getenv("SIDECHAIN_AU_CATALOG")
	paramID := os.Getenv("SIDECHAIN_AU_PARAM")
	if port == "" || catPath == "" || paramID == "" {
		t.Skip("set SIDECHAIN_AU_PORT/CATALOG/PARAM to run the AU load+drive smoke test")
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read AU catalog: %v", err)
	}
	cat, err := loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load AU catalog: %v", err)
	}
	if len(cat.All()) == 0 {
		t.Fatalf("AU catalog enumerated zero params (load likely failed)")
	}
	s := newSession(cat)
	ctx := context.Background()
	p, _ := strconv.Atoi(port)

	// connect_live handshakes with a ping; a successful connect proves the AU is loaded and the socket serves.
	cres, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: p})
	if err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	if !strings.Contains(textOf(cres), "Connected LIVE") {
		t.Fatalf("connect_live to AU host did not connect: %s", textOf(cres))
	}

	// get_param reads a real value off the running AU.
	if _, _, err := s.handleGetParam(ctx, nil, getParamIn{ID: paramID}); err != nil {
		t.Fatalf("get_param on AU: %v", err)
	}

	// set_param (normalized) forwards to the AU, and a read-back confirms the value stuck.
	half := 0.75
	sres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: paramID, Normalized: &half})
	if err != nil {
		t.Fatalf("set_param on AU: %v", err)
	}
	if !strings.Contains(textOf(sres), "Set LIVE") {
		t.Fatalf("set_param on AU did not drive the instance: %s", textOf(sres))
	}
	gres, gout, err := s.handleGetParam(ctx, nil, getParamIn{ID: paramID})
	if err != nil {
		t.Fatalf("get_param read-back on AU: %v", err)
	}
	g, ok := gout.(struct {
		Param      ParamDef `json:"param"`
		Value      float64  `json:"value"`
		Normalized float64  `json:"normalized"`
		IsSet      bool     `json:"isSet"`
		Live       bool     `json:"live"`
	})
	if !ok || !g.Live {
		t.Fatalf("AU get_param read-back not marked live: %s", textOf(gres))
	}
	// The AU applies its own normalization; assert the read-back moved toward the target (not an exact equality,
	// since an AU may quantize or clamp), which proves the round-trip reaches the real instance.
	if g.Normalized < 0.5 {
		t.Fatalf("AU set to normalized 0.75 read back at %.4f, expected it to move up-range", g.Normalized)
	}
	t.Logf("AU load+drive ok: %d params, param %q -> %s", len(cat.All()), paramID, textOf(gres))
}
