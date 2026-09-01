// concurrency_live_test.go - the C1 concurrency test level (docs/CONCURRENCY.md): multiple controllers driving
// ONE host at once. It connects several liveClients to a running host and asserts distinct identities,
// concurrent independent control, a state read during a concurrent write hammer (the case the old single-client
// machinery would have collided on), and clean per-connection disconnect. Gated on the sweep env; skipped
// otherwise, and driven by the integration workflow against a real plugin.

package sidechain

import (
	"os"
	"strconv"
	"sync"
	"testing"
)

func TestMultiClientLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_SWEEP_PORT")
	catPath := os.Getenv("SIDECHAIN_SWEEP_CATALOG")
	if portStr == "" || catPath == "" {
		t.Skip("set SIDECHAIN_SWEEP_PORT + SIDECHAIN_SWEEP_CATALOG to run the multi-controller test")
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
	var floats []string
	for _, p := range cat.All() {
		if p.Type == "float" {
			floats = append(floats, p.ID)
		}
		if len(floats) >= 2 {
			break
		}
	}
	if len(floats) < 2 {
		t.Skip("need at least two float params for the multi-controller test")
	}
	px, py := floats[0], floats[1]

	dial := func() *liveClient {
		lc, err := dialLive("127.0.0.1", port)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return lc
	}

	// Two controllers with distinct identities.
	a, b := dial(), dial()
	defer a.Close()
	defer b.Close()
	if a.clientID == 0 || b.clientID == 0 || a.clientID == b.clientID {
		t.Fatalf("expected two distinct nonzero client ids, got A=%d B=%d", a.clientID, b.clientID)
	}
	t.Logf("controllers A=%d B=%d", a.clientID, b.clientID)

	// Concurrent independent control: A drives px, B drives py; each lands.
	if _, _, _, err := a.SetParam(px, 0.30, false); err != nil {
		t.Fatalf("A set: %v", err)
	}
	if _, _, _, err := b.SetParam(py, 0.70, false); err != nil {
		t.Fatalf("B set: %v", err)
	}
	if _, n, _, _ := a.GetParam(px); n < 0.25 || n > 0.35 {
		t.Fatalf("A's param did not land: %.3f, want ~0.30", n)
	}
	if _, n, _, _ := b.GetParam(py); n < 0.65 || n > 0.75 {
		t.Fatalf("B's param did not land: %.3f, want ~0.70", n)
	}

	// Hammer from A and B concurrently while a third controller reads full state throughout. The old design
	// (one shared appliedEvent + single scratch slots) would have raced here; per-request completion must not.
	c := dial()
	defer c.Close()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var setErrs int
	hammer := func(lc *liveClient, id string) {
		defer wg.Done()
		for i := 0; i < 150; i++ {
			if _, _, _, err := lc.SetParam(id, float64(i%100)/100.0, false); err != nil {
				mu.Lock()
				setErrs++
				mu.Unlock()
			}
		}
	}
	wg.Add(2)
	go hammer(a, px)
	go hammer(b, py)
	stateOK := 0
	for i := 0; i < 20; i++ {
		if s, err := c.GetFullState(); err == nil && s != "" {
			stateOK++
		}
	}
	wg.Wait()
	if setErrs != 0 {
		t.Fatalf("concurrent hammer had %d set errors", setErrs)
	}
	if stateOK != 20 {
		t.Fatalf("concurrent get_full_state during hammer: %d/20 ok", stateOK)
	}

	// Disconnect A; B and C keep working (clean per-connection teardown, no stall).
	a.Close()
	if _, _, _, err := b.GetParam(px); err != nil {
		t.Fatalf("B failed after A disconnected: %v", err)
	}
	t.Logf("multi-controller OK: distinct ids, independent control, 300 concurrent sets, 20/20 state reads, clean disconnect")
}
