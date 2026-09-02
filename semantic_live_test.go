// semantic_live_test.go - gated real-host test of the Phase 3 store: a real plugin's inference is probed once,
// persisted under its identity fingerprint, and RECALLED by a second, HEADLESS session (no live connection), which
// is the whole point - probing is paid once ever. Gated on the sweep env; run by drive_plugin.sh per plugin.

package sidechain

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestSemanticStoreLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_SWEEP_PORT")
	catPath := os.Getenv("SIDECHAIN_SWEEP_CATALOG")
	if portStr == "" || catPath == "" {
		t.Skip("set SIDECHAIN_SWEEP_PORT + SIDECHAIN_SWEEP_CATALOG to run the semantic-store live test")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port: %v", err)
	}
	data, err := os.ReadFile(catPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	cat, err := loadCatalogJSON(data)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if cat.Plugin.Name == "" {
		t.Fatal("catalog carries no plugin identity - the host must emit it for the fingerprint")
	}
	var target string
	for _, p := range cat.All() {
		if p.Type == "float" {
			target = p.ID
			break
		}
	}
	if target == "" {
		t.Skip("no float param to probe")
	}

	dir := t.TempDir()
	ctx := context.Background()

	// Session 1: connect, describe (probes + persists).
	s := newSession(cat)
	if err := s.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatalf("attach store: %v", err)
	}
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: port}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	_, dout, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: target})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if dout.(describeParamOut).Cached {
		t.Fatal("the first describe should probe, not recall")
	}
	s.handleDisconnectLive(ctx, nil, emptyIn{})

	// Session 2: HEADLESS (never connects). It must recall the persisted inference from the store.
	s2 := newSession(cat)
	if err := s2.attachStore(NewSemanticStore(dir)); err != nil {
		t.Fatalf("attach store 2: %v", err)
	}
	_, dout2, err := s2.handleDescribeParam(ctx, nil, describeParamIn{ID: target})
	if err != nil {
		t.Fatalf("headless describe: %v", err)
	}
	d2 := dout2.(describeParamOut)
	if !d2.Cached {
		t.Fatalf("the headless second-session describe must recall from the store, got %+v", d2)
	}
	fp := fingerprintCatalog(cat)
	t.Logf("semantic store live OK: %s param %q probed then recalled headless (behaviorClass %q, fingerprint %s)",
		cat.Plugin.Name, target, d2.BehaviorClass, fp[:19])
}
