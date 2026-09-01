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
	"time"
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

// TestChangeNotifications is the C2 concurrency test level (docs/CONCURRENCY.md): a change by one controller
// becomes visible to another as a pushed param_changed event. Controller A sets a param; controller B, which
// issued no request, must receive an event on its Events() channel carrying the param, the new value/text, and
// the originator's clientID (attribution). This exercises the multiplexed async protocol end to end: the host's
// AudioProcessorParameter::Listener broadcast, the per-connection outbound queue, and B's reader/demux routing
// the unsolicited message to Events() rather than a reply. Gated on the sweep env; skipped otherwise.
func TestChangeNotifications(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_SWEEP_PORT")
	catPath := os.Getenv("SIDECHAIN_SWEEP_CATALOG")
	if portStr == "" || catPath == "" {
		t.Skip("set SIDECHAIN_SWEEP_PORT + SIDECHAIN_SWEEP_CATALOG to run the change-notification test")
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
	var target string
	for _, p := range cat.All() {
		if p.Type == "float" {
			target = p.ID
			break
		}
	}
	if target == "" {
		t.Skip("need at least one float param for the change-notification test")
	}

	dial := func() *liveClient {
		lc, err := dialLive("127.0.0.1", port)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return lc
	}

	// A drives, B watches. B issues no request; it must still learn of A's change.
	a, b := dial(), dial()
	defer a.Close()
	defer b.Close()
	if a.clientID == 0 || b.clientID == 0 || a.clientID == b.clientID {
		t.Fatalf("expected two distinct nonzero client ids, got A=%d B=%d", a.clientID, b.clientID)
	}

	// Move the param to a fresh value (avoid a no-op set that emits no change). Read first, then pick a target
	// that differs.
	_, cur, _, _ := a.GetParam(target)
	want := 0.65
	if cur > 0.55 && cur < 0.75 {
		want = 0.20
	}

	// Drain any events queued on B before we act (e.g. from an earlier test on a shared host).
	for drained := true; drained; {
		select {
		case <-b.Events():
		default:
			drained = false
		}
	}

	if _, _, _, err := a.SetParam(target, want, false); err != nil {
		t.Fatalf("A set: %v", err)
	}

	// B must receive a param_changed event attributed to A, for the param A moved, within a short window.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-b.Events():
			if ev["event"] != "param_changed" {
				continue
			}
			if p, _ := ev["param"].(string); p != target {
				continue // an unrelated param (e.g. a plugin that ganged a change); keep waiting for ours
			}
			by, _ := ev["by"].(float64)
			if int(by) != a.clientID {
				t.Fatalf("event attribution wrong: by=%d, want A=%d", int(by), a.clientID)
			}
			norm, _ := ev["normalized"].(float64)
			if norm < want-0.05 || norm > want+0.05 {
				t.Fatalf("event normalized=%.3f, want ~%.3f", norm, want)
			}
			if _, ok := ev["text"].(string); !ok {
				t.Fatalf("event missing value text: %v", ev)
			}
			t.Logf("change-notification OK: B(%d) saw param_changed{param:%s, normalized:%.3f, by:%d}", b.clientID, target, norm, int(by))
			return
		case <-deadline:
			t.Fatalf("B did not receive a param_changed event for %s within the window", target)
		}
	}
}

// TestGovernedLive exercises the C3 governed conflict tier wired into ControlServer's message-thread drain
// (GovernedState.h). Two controllers drive the hierarchical edit leases over the socket: A takes the whole-instance
// lease (applied); B's acquire is rejected while A holds it, and B cannot take a section of A's instance (the
// reject guards); after A releases, A and B take different sections, then A takes the whole instance, which is
// compensated (B's section revoked, A's kept); a load_state bumps the patch generation and B observes it as a
// pushed governed_changed event. Gated on the sweep env; skipped otherwise.
func TestGovernedLive(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_SWEEP_PORT")
	if portStr == "" || os.Getenv("SIDECHAIN_SWEEP_CATALOG") == "" {
		t.Skip("set SIDECHAIN_SWEEP_PORT + SIDECHAIN_SWEEP_CATALOG to run the governed-state test")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port: %v", err)
	}

	dial := func() *liveClient {
		lc, err := dialLive("127.0.0.1", port)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return lc
	}
	governed := func(m map[string]any) map[string]any {
		g, _ := m["governed"].(map[string]any)
		return g
	}
	sectionHolder := func(m map[string]any, group string) int {
		secs, _ := governed(m)["section_leases"].(map[string]any)
		h, ok := secs[group]
		if !ok {
			return 0 // absent == free
		}
		return int(h.(float64))
	}
	govern := func(lc *liveClient, req map[string]any) map[string]any {
		req["cmd"] = "govern"
		resp, err := lc.request(req)
		if err != nil {
			t.Fatalf("govern %v: %v", req, err)
		}
		return resp
	}

	a, b := dial(), dial()
	defer a.Close()
	defer b.Close()
	if a.clientID == 0 || b.clientID == 0 || a.clientID == b.clientID {
		t.Fatalf("expected two distinct nonzero client ids, got A=%d B=%d", a.clientID, b.clientID)
	}

	// Discover the leasable sections (the plugin's param groups) and pick two.
	gv, err := a.request(map[string]any{"cmd": "get_governed"})
	if err != nil {
		t.Fatalf("get_governed: %v", err)
	}
	secList, _ := gv["sections"].([]any)
	if len(secList) < 2 {
		t.Skip("plugin exposes fewer than two param groups; section-lease test needs at least two")
	}
	grpA, grpB := secList[0].(string), secList[1].(string)

	// A takes the whole instance; B's acquire is rejected while A holds it.
	if resp := govern(a, map[string]any{"op": "acquire_instance"}); resp["resolution"] != "applied" ||
		int(governed(resp)["instance_lease"].(float64)) != a.clientID {
		t.Fatalf("A should hold the instance: %v", resp)
	}
	if resp := govern(b, map[string]any{"op": "acquire_instance"}); resp["resolution"] != "rejected" {
		t.Fatalf("B's instance acquire should be rejected: %v", resp)
	}
	// B cannot take a section of an instance A holds (the hierarchy guard).
	if resp := govern(b, map[string]any{"op": "acquire_section", "group": grpA}); resp["resolution"] != "rejected" {
		t.Fatalf("B should not take a section of A's instance: %v", resp)
	}

	// A releases; now A and B take different sections (by group name).
	govern(a, map[string]any{"op": "release_instance"})
	if resp := govern(a, map[string]any{"op": "acquire_section", "group": grpA}); resp["resolution"] != "applied" || sectionHolder(resp, grpA) != a.clientID {
		t.Fatalf("A should take section %q: %v", grpA, resp)
	}
	if resp := govern(b, map[string]any{"op": "acquire_section", "group": grpB}); resp["resolution"] != "applied" || sectionHolder(resp, grpB) != b.clientID {
		t.Fatalf("B should take section %q: %v", grpB, resp)
	}

	// A now takes the whole instance: compensated (B's section revoked, A's section kept).
	resp := govern(a, map[string]any{"op": "acquire_instance"})
	if resp["resolution"] != "compensated" || int(governed(resp)["instance_lease"].(float64)) != a.clientID ||
		sectionHolder(resp, grpB) != 0 || sectionHolder(resp, grpA) != a.clientID {
		t.Fatalf("A taking the instance should compensate by revoking B's section: %v", resp)
	}

	// A load_state is a whole-patch change: the governed generation bumps and B observes it as an event.
	before, _ := a.request(map[string]any{"cmd": "get_governed"})
	gen0 := int(governed(before)["generation"].(float64))
	blob, err := a.GetFullState()
	if err != nil {
		t.Fatalf("A get_full_state: %v", err)
	}
	if err := a.LoadState(blob); err != nil {
		t.Fatalf("A load_state: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-b.Events():
			if ev["event"] != "governed_changed" {
				continue
			}
			if gen := int(governed(ev)["generation"].(float64)); gen > gen0 {
				t.Logf("governed OK: group-bound section leases %q/%q (reject+compensate), load_state bumped generation %d->%d, B(%d) saw governed_changed", grpA, grpB, gen0, gen, b.clientID)
				return
			}
		case <-deadline:
			t.Fatalf("B did not receive the governed_changed generation bump within the window")
		}
	}
}

// TestGovernedDisconnectFreesLease checks the crash-safe cleanup wired into ControlServer: a controller that holds
// the whole-instance lease and then disconnects has that lease released automatically, so another controller can
// acquire it. Gated on the sweep env.
func TestGovernedDisconnectFreesLease(t *testing.T) {
	portStr := os.Getenv("SIDECHAIN_SWEEP_PORT")
	if portStr == "" || os.Getenv("SIDECHAIN_SWEEP_CATALOG") == "" {
		t.Skip("set SIDECHAIN_SWEEP_PORT + SIDECHAIN_SWEEP_CATALOG to run the governed-disconnect test")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("bad port: %v", err)
	}
	dial := func() *liveClient {
		lc, err := dialLive("127.0.0.1", port)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return lc
	}
	governed := func(m map[string]any) map[string]any {
		g, _ := m["governed"].(map[string]any)
		return g
	}

	a, b := dial(), dial()
	defer b.Close()

	// A takes the whole instance, then disconnects. The server must release A's lease.
	if resp, err := a.request(map[string]any{"cmd": "govern", "op": "acquire_instance"}); err != nil ||
		int(governed(resp)["instance_lease"].(float64)) != a.clientID {
		t.Fatalf("A should hold the instance before disconnect: %v (err %v)", resp, err)
	}
	a.Close()

	// B may now acquire the instance. The cleanup is async on the server's message thread, so poll briefly.
	deadline := time.After(3 * time.Second)
	for {
		resp, err := b.request(map[string]any{"cmd": "govern", "op": "acquire_instance"})
		if err == nil && resp["resolution"] == "applied" && int(governed(resp)["instance_lease"].(float64)) == b.clientID {
			t.Logf("governed disconnect OK: A(%d)'s instance lease was freed on disconnect, B(%d) acquired it", a.clientID, b.clientID)
			return
		}
		// Not yet freed: release B's (rejected/no-op) attempt state is unchanged; retry until the cleanup lands.
		select {
		case <-deadline:
			t.Fatalf("A's lease was not freed on disconnect within the window (last resp %v)", resp)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
