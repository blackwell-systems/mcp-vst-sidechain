// live_test.go - loopback test for the live-control path. Stands up a FAKE host listener (a goroutine speaking
// the ControlServer line-JSON protocol against an in-memory param map) and drives it through the session's
// live path: connect_live -> set_param forwards + the fake records it -> get_param reads it back -> play_note
// reaches the fake -> all_notes_off panics it -> disconnect_live returns to headless.
//
// This exercises the Go forwarder end to end without a running C++ host; the real cpp/ControlServer.h speaks
// the identical protocol, so this is "same bytes on the socket."

package sidechain

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeHz renders a normalized value as a linear 20..20000 Hz string, so the fake host has a param whose value
// text carries a real unit (like a real plugin's getText) for the Phase-1 probe tests. The "expo" param id
// renders a log-frequency 20..20000 Hz sweep instead, to exercise binary-search refinement (a linear seed is
// far off on that curve).
func fakeHz(norm float64) string     { return fmt.Sprintf("%.2f Hz", 20+norm*19980) }
func fakeHzExpo(norm float64) string { return fmt.Sprintf("%.2f Hz", 20*math.Pow(1000, norm)) }

// fakeHzSigmoid is a monotonic S-curve (a logistic): its concave-then-convex shape fits none of the
// linear/exp/power models well, so it forces the binary-search refinement path. real(0) > 0 (so it is not a
// zero-crossing power law) and it is flat at both ends but steep in the middle.
func fakeHzSigmoid(norm float64) float64 { return 1000 / (1 + math.Exp(-12*(norm-0.5))) }

func renderFor(id string, norm float64) string {
	switch id {
	case "expo":
		return fakeHzExpo(norm)
	case "power": // zero-crossing power law real = 32*norm^6.5 s, rendered ms below 1 s, s at/above (a time knob).
		// Full precision (no rounding) so the samples lie exactly on the curve and the power fit is clean.
		v := 32 * math.Pow(norm, 6.5)
		if v < 1 {
			return fmt.Sprintf("%g ms", v*1000)
		}
		return fmt.Sprintf("%g s", v)
	case "sigmoid": // S-curve: fits no closed-form model => forces binary-search refinement
		return fmt.Sprintf("%.2f Hz", fakeHzSigmoid(norm))
	case "toggle": // discrete: labels only, so inference reports !Numeric
		if norm >= 0.5 {
			return "On"
		}
		return "Off"
	case "filterType": // discrete-as-float: three bands render LP/BP/HP across thirds of the range
		switch {
		case norm < 1.0/3.0:
			return "LP"
		case norm < 2.0/3.0:
			return "BP"
		default:
			return "HP"
		}
	case "raw": // numeric but unitless: getText echoes the bare number, so inference has Unit==""
		return fmt.Sprintf("%.4f", norm)
	default:
		return fakeHz(norm)
	}
}

// fakeHost is a minimal stand-in for cpp/ControlServer: it accepts one client and answers the same commands
// over the same line-delimited JSON protocol, backed by an in-memory normalized-value map.
type fakeHost struct {
	ln            net.Listener
	mu            sync.Mutex
	params        map[string]float64 // id -> normalized (set_param with `normalized`)
	reals         map[string]float64 // id -> real value (set_param with `value`; skew-aware forwards land here)
	notes         []int              // notes turned on
	panics        int                // all_notes_off count
	renders       int                // render count
	lastNoteCount int                // note count from the last phrase render (0 if a single-note render)
	state         string             // last-saved / loaded opaque state
	resets        int                // reset_init count
	cmds          map[string]int     // per-command call counts (e.g. how many get_param probes the sweep issued)

	// C2/C3: identity + a minimal in-memory governed model (enough to exercise the governed MCP tools; the full
	// conflict tier is proven in governed_test.go and the gated live tests).
	nextClient int
	instance   int            // whole-instance lease holder; 0 = free
	sections   map[string]int // param group -> holder
	generation int            // bumped on a whole-patch change (load_state)
	leasable   []string       // the leasable sections this fake exposes
}

func startFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake host listen: %v", err)
	}
	fh := &fakeHost{
		ln: ln, params: map[string]float64{}, reals: map[string]float64{}, state: "<STATE/>", cmds: map[string]int{},
		sections: map[string]int{}, leasable: []string{"Amp", "Filter", "Osc"},
	}
	go fh.serve()
	return fh
}

func (fh *fakeHost) port() int { return fh.ln.Addr().(*net.TCPAddr).Port }
func (fh *fakeHost) stop()     { fh.ln.Close() }

func (fh *fakeHost) serve() {
	for {
		conn, err := fh.ln.Accept()
		if err != nil {
			return
		}
		go fh.handle(conn)
	}
}

func (fh *fakeHost) handle(conn net.Conn) {
	defer conn.Close()
	fh.mu.Lock()
	fh.nextClient++
	client := fh.nextClient
	fh.mu.Unlock()
	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return
		}
		var req map[string]any
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		resp := fh.dispatch(req, client)
		if id, ok := req["id"]; ok {
			resp["id"] = id
		}
		b, _ := json.Marshal(resp)
		conn.Write(append(b, '\n'))
	}
}

// govObj builds the wire `governed` object from the fake's in-memory state.
func (fh *fakeHost) govObj() map[string]any {
	secs := map[string]any{}
	for n, h := range fh.sections {
		secs[n] = h
	}
	return map[string]any{"instance_lease": fh.instance, "section_leases": secs, "generation": fh.generation}
}

func fhContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (fh *fakeHost) dispatch(req map[string]any, client int) map[string]any {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	cmd, _ := req["cmd"].(string)
	fh.cmds[cmd]++
	switch cmd {
	case "ping":
		return map[string]any{"ok": true, "pong": true, "client": client}
	case "govern":
		op, _ := req["op"].(string)
		grp, _ := req["group"].(string)
		switch op {
		case "acquire_instance":
			if fh.instance != 0 && fh.instance != client {
				return map[string]any{"ok": true, "resolution": "rejected", "governed": fh.govObj()}
			}
			fh.instance = client
			res := "applied"
			for n, h := range fh.sections {
				if h != client {
					delete(fh.sections, n)
					res = "compensated"
				}
			}
			return map[string]any{"ok": true, "resolution": res, "governed": fh.govObj()}
		case "release_instance":
			if fh.instance == client {
				fh.instance = 0
			}
			return map[string]any{"ok": true, "resolution": "applied", "governed": fh.govObj()}
		case "acquire_section":
			if !fhContains(fh.leasable, grp) {
				return map[string]any{"ok": false, "error": "unknown_section"}
			}
			if fh.instance != 0 && fh.instance != client {
				return map[string]any{"ok": true, "resolution": "rejected", "governed": fh.govObj()}
			}
			if h, ok := fh.sections[grp]; ok && h != client {
				return map[string]any{"ok": true, "resolution": "rejected", "governed": fh.govObj()}
			}
			fh.sections[grp] = client
			return map[string]any{"ok": true, "resolution": "applied", "governed": fh.govObj()}
		case "release_section":
			if h, ok := fh.sections[grp]; ok && h == client {
				delete(fh.sections, grp)
			}
			return map[string]any{"ok": true, "resolution": "applied", "governed": fh.govObj()}
		}
		return map[string]any{"ok": false, "error": "unknown_op"}
	case "get_governed":
		secs := make([]any, len(fh.leasable))
		for i, s := range fh.leasable {
			secs[i] = s
		}
		return map[string]any{"ok": true, "governed": fh.govObj(), "sections": secs}
	case "set_param":
		id, _ := req["param"].(string)
		if rv, ok := req["value"].(float64); ok { // real-unit forward (HasRealRange param)
			fh.reals[id] = rv
			return map[string]any{"ok": true, "param": id, "normalized": rv, "value": rv, "text": "ok"}
		}
		norm, _ := req["normalized"].(float64)
		fh.params[id] = norm
		return map[string]any{"ok": true, "param": id, "normalized": norm, "value": norm, "text": renderFor(id, norm)}
	case "get_param":
		id, _ := req["param"].(string)
		norm := fh.params[id]
		return map[string]any{"ok": true, "param": id, "normalized": norm, "value": norm, "text": renderFor(id, norm)}
	case "get_full_state":
		return map[string]any{"ok": true, "state": fh.state}
	case "load_state":
		st, _ := req["state"].(string)
		fh.state = st
		fh.generation++ // a whole-patch change bumps the governed generation (mirrors the real host)
		return map[string]any{"ok": true, "loaded": true}
	case "reset_init":
		fh.resets++
		return map[string]any{"ok": true, "reset": true}
	case "note_on":
		n, _ := req["note"].(float64)
		fh.notes = append(fh.notes, int(n))
		return map[string]any{"ok": true, "note": int(n), "on": true}
	case "note_off":
		return map[string]any{"ok": true, "on": false}
	case "all_notes_off":
		fh.panics++
		return map[string]any{"ok": true}
	case "render":
		// Deterministic measurement so the render_and_measure AND tune_param tools are testable in-memory (the fake
		// has no DSP; audio correctness lives only in the gated real-host tests). The spectral centroid RESPONDS to
		// the "cutoff" param (monotonic 200..4000 Hz across its normalized range), so a tune toward brightness has a
		// real gradient to climb; the other numbers are canned. Mirrors the wire contract shape.
		//
		// When the request carries "temporal": true, the reply also includes a "modulation" block whose
		// centroid.rate_hz responds to the cutoff param (1 + cutoff*9, so 1..10 Hz), making it tunable. A "vibrato"
		// param drives pitch.rate_hz (1 + vibrato*9) and pitch.depth (vibrato*2 semitones); when vibrato is 0, the
		// pitch block is a low/irregular stub. dominant stays "centroid" so existing modulation tests are stable.
		// When temporal is absent/false, the modulation block is omitted so existing non-temporal tests are stable.
		//
		// A "notes" array in the request is accepted without error (the fake has no DSP; it just records the count).
		// "mpe" is also accepted without error.
		fh.renders++
		cutoff := fh.params["cutoff"] // dispatch already holds fh.mu
		centroid := 200.0 + cutoff*3800.0
		// rms_db responds to a "gain" param (default 0 keeps the historical -18.4), giving tune_params a SECOND
		// independent axis to co-optimize (cutoff -> centroid, gain -> rms) in the in-memory tests.
		rmsDb := -18.4 + fh.params["gain"]*18.0
		meas := map[string]any{
			"duration_sec": 2.0, "sample_rate": 48000.0, "channels": 2,
			"peak_db": -6.2, "rms_db": rmsDb, "crest": 12.2, "centroid_hz": centroid,
			"bands":  map[string]any{"low_db": -20.1, "mid_db": -16.8, "high_db": -28.0},
			"silent": false, "clipped": false,
		}
		// Record phrase note count if the request included a notes array.
		if notesArr, ok := req["notes"].([]any); ok {
			fh.lastNoteCount = len(notesArr)
		} else {
			fh.lastNoteCount = 0
		}
		if temporal, _ := req["temporal"].(bool); temporal {
			frameMs := 25
			if f, ok := req["frame_ms"].(float64); ok && f > 0 {
				frameMs = int(f)
			}
			rate := 1.0 + cutoff*9.0 // 1..10 Hz, monotonic in cutoff
			depth := cutoff * 2000.0 // 0..2000 Hz depth
			vibrato := fh.params["vibrato"]
			var pitchBlock map[string]any
			if vibrato > 0 {
				// Vibrato param drives pitch modulation: rate = 1 + vibrato*9 Hz, depth = vibrato*2 semitones.
				pitchBlock = map[string]any{
					"rate_hz": 1.0 + vibrato*9.0, "depth": vibrato * 2.0, "regular": true, "confidence": 0.9,
				}
			} else {
				// No vibrato: low/irregular pitch stub.
				pitchBlock = map[string]any{
					"rate_hz": 0.0, "depth": 0.0, "regular": false, "confidence": 0.05,
				}
			}
			meas["modulation"] = map[string]any{
				"frame_ms": frameMs,
				"centroid": map[string]any{
					"rate_hz": rate, "depth": depth, "regular": true, "confidence": 0.9,
				},
				"rms": map[string]any{
					"rate_hz": 0.0, "depth": 1.2, "regular": false, "confidence": 0.1,
				},
				"pitch":    pitchBlock,
				"dominant": "centroid",
			}
		}
		return map[string]any{"ok": true, "measurement": meas}
	}
	return map[string]any{"ok": false, "error": "unknown_cmd"}
}

// newLiveTestSession builds a session with a tiny synthetic catalog (one float param) so set/get_param resolve.
func newLiveTestSession() *session {
	cat := NewCatalog([]ParamDef{
		{ID: "cutoff", Label: "Cutoff", Type: "float", Min: 20, Max: 20000, Default: 1000, Group: "filter"},
	})
	return newSession(cat)
}

func TestLiveForwarding(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()

	// connect_live
	res, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()})
	if err != nil {
		t.Fatalf("connect_live err: %v", err)
	}
	if got := textOf(res); !strings.Contains(got, "Connected LIVE") {
		t.Fatalf("connect_live reply = %q", got)
	}
	if s.live == nil {
		t.Fatal("session not marked live after connect_live")
	}

	// set_param (max value) forwards to the fake host, NOT the headless params.
	val := 20000.0
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Value: &val}); err != nil {
		t.Fatalf("set_param err: %v", err)
	}
	fh.mu.Lock()
	gotNorm, ok := fh.params["cutoff"]
	fh.mu.Unlock()
	if !ok || gotNorm < 0.999 {
		t.Fatalf("fake host did not receive the max-cutoff set (norm=%v ok=%v)", gotNorm, ok)
	}
	if _, headless := s.params["cutoff"]; headless {
		t.Fatal("live set_param should NOT have mutated the headless session params")
	}

	// get_param reads it back off the fake host (Live:true).
	gres, gout, err := s.handleGetParam(ctx, nil, getParamIn{ID: "cutoff"})
	if err != nil {
		t.Fatalf("get_param err: %v", err)
	}
	if !strings.Contains(textOf(gres), "LIVE") {
		t.Fatalf("get_param not live-marked: %q", textOf(gres))
	}
	if g, _ := gout.(struct {
		Param      ParamDef `json:"param"`
		Value      float64  `json:"value"`
		Normalized float64  `json:"normalized"`
		IsSet      bool     `json:"isSet"`
		Live       bool     `json:"live"`
	}); !g.Live {
		t.Fatal("get_param structured output not marked Live")
	}

	// play_note reaches the fake host.
	if _, _, err := s.handlePlayNote(ctx, nil, playNoteIn{Note: 60, Vel: 0.9}); err != nil {
		t.Fatalf("play_note err: %v", err)
	}
	fh.mu.Lock()
	nNotes := len(fh.notes)
	first := -1
	if nNotes > 0 {
		first = fh.notes[0]
	}
	fh.mu.Unlock()
	if nNotes != 1 || first != 60 {
		t.Fatalf("fake host notes = %v, want [60]", fh.notes)
	}

	// all_notes_off panics the fake host.
	if _, _, err := s.handleAllNotesOff(ctx, nil, emptyIn{}); err != nil {
		t.Fatalf("all_notes_off err: %v", err)
	}
	fh.mu.Lock()
	panics := fh.panics
	fh.mu.Unlock()
	if panics != 1 {
		t.Fatalf("all_notes_off count = %d, want 1", panics)
	}

	// disconnect_live returns to headless; set_param now mutates the session again.
	if _, _, err := s.handleDisconnectLive(ctx, nil, emptyIn{}); err != nil {
		t.Fatalf("disconnect_live err: %v", err)
	}
	if s.live != nil {
		t.Fatal("session still live after disconnect_live")
	}
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Value: &val}); err != nil {
		t.Fatalf("headless set_param err: %v", err)
	}
	if _, headless := s.params["cutoff"]; !headless {
		t.Fatal("headless set_param after disconnect should mutate session params")
	}
}

// TestLiveRealRangeForwarding proves a HasRealRange param forwards its REAL value (so the plugin applies its own
// skew), while a plain hosted param still forwards a normalized 0..1. This is the fidelity fix: the Go layer
// never linearises a curve it does not own.
func TestLiveRealRangeForwarding(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	cat := NewCatalog([]ParamDef{
		{ID: "cutoff", Label: "Cutoff", Type: "float", Min: 20, Max: 20000, Default: 1000, Group: "filter", HasRealRange: true},
		{ID: "mix", Label: "Mix", Type: "float", Min: 0, Max: 1, Default: 0.5, Group: "amp"}, // hosted: no real range
	})
	s := newSession(cat)
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// Real-value set on the ranged param -> forwarded as a REAL value (not linear-normalized).
	cutoff := 500.0
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Value: &cutoff}); err != nil {
		t.Fatalf("set_param cutoff: %v", err)
	}
	fh.mu.Lock()
	gotReal, sawReal := fh.reals["cutoff"]
	_, sawNorm := fh.params["cutoff"]
	fh.mu.Unlock()
	if !sawReal || gotReal != 500 {
		t.Fatalf("ranged param should forward real value 500, got real=%v ok=%v", gotReal, sawReal)
	}
	if sawNorm {
		t.Fatal("ranged param must NOT forward a (linear) normalized value")
	}

	// A normalized set on the ranged param -> forwarded as normalized (it is already plugin-space 0..1).
	half := 0.25
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Normalized: &half}); err != nil {
		t.Fatalf("set_param cutoff normalized: %v", err)
	}
	fh.mu.Lock()
	gotNorm := fh.params["cutoff"]
	fh.mu.Unlock()
	if gotNorm != 0.25 {
		t.Fatalf("normalized set should forward 0.25 verbatim, got %v", gotNorm)
	}

	// Hosted param (no real range): a value set forwards as normalized.
	mix := 0.8
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "mix", Value: &mix}); err != nil {
		t.Fatalf("set_param mix: %v", err)
	}
	fh.mu.Lock()
	_, mixReal := fh.reals["mix"]
	mixNorm := fh.params["mix"]
	fh.mu.Unlock()
	if mixReal {
		t.Fatal("hosted param must forward normalized, not a real value")
	}
	if mixNorm != 0.8 {
		t.Fatalf("hosted param value 0.8 -> normalized 0.8, got %v", mixNorm)
	}
}

// TestLiveStateVerbs exercises save_state / load_state / reset_init against the fake host.
func TestLiveStateVerbs(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession()
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// save_state returns the host's opaque blob.
	res, out, err := s.handleSaveState(ctx, nil, emptyIn{})
	if err != nil {
		t.Fatalf("save_state: %v", err)
	}
	if !strings.Contains(textOf(res), "Saved live state") {
		t.Fatalf("save_state reply = %q", textOf(res))
	}
	saved, _ := out.(struct {
		State string `json:"state"`
	})
	if saved.State != "<STATE/>" {
		t.Fatalf("save_state blob = %q, want <STATE/>", saved.State)
	}

	// load_state pushes a new blob.
	if _, _, err := s.handleLoadState(ctx, nil, loadStateIn{State: "<PATCH x=\"1\"/>"}); err != nil {
		t.Fatalf("load_state: %v", err)
	}
	fh.mu.Lock()
	gotState := fh.state
	fh.mu.Unlock()
	if gotState != "<PATCH x=\"1\"/>" {
		t.Fatalf("host state = %q after load_state", gotState)
	}

	// reset_init reaches the host.
	if _, _, err := s.handleResetInit(ctx, nil, emptyIn{}); err != nil {
		t.Fatalf("reset_init: %v", err)
	}
	fh.mu.Lock()
	resets := fh.resets
	fh.mu.Unlock()
	if resets != 1 {
		t.Fatalf("reset_init count = %d, want 1", resets)
	}

	// Guard: state verbs require a live connection.
	if _, _, err := s.handleDisconnectLive(ctx, nil, emptyIn{}); err != nil {
		t.Fatalf("disconnect_live: %v", err)
	}
	if r, _, _ := s.handleSaveState(ctx, nil, emptyIn{}); !strings.Contains(textOf(r), "not live") {
		t.Fatalf("save_state offline = %q, want not-live guard", textOf(r))
	}
}

// TestDescribeAndSetReal exercises the wired Phase-1 path: describe_param probes the fake host's value text and
// recovers the real unit/range/curve, then set_param real= maps a real target back to the right normalized
// position. The fake renders a linear 20..20000 Hz surface (see fakeHz).
func TestDescribeAndSetReal(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession() // cutoff: min 20, max 20000, HasRealRange false (hosted-style)
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// describe_param recovers unit + linear curve from the value-text sweep.
	dres, dout, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: "cutoff"})
	if err != nil {
		t.Fatalf("describe_param: %v", err)
	}
	sum := strings.ToLower(textOf(dres))
	if !strings.Contains(sum, "hz") || !strings.Contains(sum, "linear") {
		t.Fatalf("describe summary = %q, want hz + linear", textOf(dres))
	}
	if d, ok := dout.(describeParamOut); !ok || d.Inference.Unit != "hz" || !approx(d.Inference.RealMax, 20000, 1) {
		t.Fatalf("describe structured = %+v", dout)
	}

	// set_param real=1000 Hz -> normalized ~ (1000-20)/19980 = 0.04905.
	target := 1000.0
	sres, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Real: &target})
	if err != nil {
		t.Fatalf("set_param real: %v", err)
	}
	if !strings.Contains(textOf(sres), "Hz") {
		t.Fatalf("set real reply = %q, want a Hz readback", textOf(sres))
	}
	fh.mu.Lock()
	gotNorm := fh.params["cutoff"]
	fh.mu.Unlock()
	if !approx(gotNorm, 0.04905, 0.005) {
		t.Fatalf("set real landed at norm %.4f, want ~0.049", gotNorm)
	}

	// real is mutually exclusive with normalized.
	n := 0.5
	if r, _, _ := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Real: &target, Normalized: &n}); !strings.Contains(textOf(r), "mutually exclusive") {
		t.Fatalf("real+normalized should be rejected, got %q", textOf(r))
	}
}

// TestSetRealExponentialAnalytic: a perfect log-frequency curve gets an exp fit, so set_param real= inverts in
// closed form (no search) and lands exact.
func TestSetRealExponentialAnalytic(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newSession(NewCatalog([]ParamDef{{ID: "expo", Label: "Cutoff", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	_, dout, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: "expo"})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	d, _ := dout.(describeParamOut)
	if d.Inference.Fit == nil || d.Inference.Fit.Model != "exp" || !d.Inference.analyticReliable() {
		t.Fatalf("expo should get a reliable exp fit, got %+v", d.Inference.Fit)
	}

	target := 1000.0
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "expo", Real: &target}); err != nil {
		t.Fatalf("set real: %v", err)
	}
	fh.mu.Lock()
	landedNorm := fh.params["expo"]
	fh.mu.Unlock()
	if landedHz := 20 * math.Pow(1000, landedNorm); math.Abs(landedHz-target) > 2 {
		t.Fatalf("analytic exp landed at %.2f Hz (norm %.4f), want ~1000", landedHz, landedNorm)
	}
}

// TestSetRealPowerAnalytic: a zero-crossing time curve (real = 32*norm^6.5 s, rendered ms/s) gets a power fit,
// so set_param real= inverts in closed form (no binary-search refinement) and lands on the target.
func TestSetRealPowerAnalytic(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newSession(NewCatalog([]ParamDef{{ID: "power", Label: "Attack", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	_, dout, err := s.handleDescribeParam(ctx, nil, describeParamIn{ID: "power"})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	d, _ := dout.(describeParamOut)
	if d.Inference.Unit != "s" || d.Inference.Fit == nil || d.Inference.Fit.Model != "power" || !d.Inference.analyticReliable() {
		t.Fatalf("power param should get a reliable power fit in s, got unit=%q fit=%+v", d.Inference.Unit, d.Inference.Fit)
	}

	target := 4.0 // 32*norm^6.5 = 4 => norm = (4/32)^(1/6.5) = 0.7286
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "power", Real: &target}); err != nil {
		t.Fatalf("set real: %v", err)
	}
	fh.mu.Lock()
	landedNorm := fh.params["power"]
	fh.mu.Unlock()
	if landedSec := 32 * math.Pow(landedNorm, 6.5); math.Abs(landedSec-target) > 0.05 {
		t.Fatalf("analytic power landed at %.3f s (norm %.4f), want ~4", landedSec, landedNorm)
	}
}

// TestSetRealRefineFallback: an S-curve (logistic) fits none of the linear/exp/power models, so
// analyticReliable() is false and set_param real= must fall back to binary-search refinement to converge.
func TestSetRealRefineFallback(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newSession(NewCatalog([]ParamDef{{ID: "sigmoid", Label: "Weird", Type: "float", Min: 0, Max: 1}}))
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}
	if _, err := s.inference("sigmoid"); err != nil { // prime the cache so we can inspect the fit
		t.Fatalf("probe sigmoid: %v", err)
	}
	if s.infer["sigmoid"].analyticReliable() {
		t.Fatalf("sigmoid should NOT get a reliable fit, got %+v", s.infer["sigmoid"].Fit)
	}
	target := 500.0 // logistic midpoint: sigmoid(0.5) = 500 => n = 0.5
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "sigmoid", Real: &target}); err != nil {
		t.Fatalf("set real: %v", err)
	}
	fh.mu.Lock()
	landedNorm := fh.params["sigmoid"]
	fh.mu.Unlock()
	if landedHz := fakeHzSigmoid(landedNorm); math.Abs(landedHz-target) > 5 {
		t.Fatalf("refinement landed at %.2f Hz (norm %.4f), want ~500", landedHz, landedNorm)
	}
}

// TestRealSetCachesInference proves the per-session probe cache: two real-unit sets on the SAME param probe the
// plugin only ONCE. The value-text sweep (SampleText) opens with exactly one get_param (its "orig" read), so
// counting get_param calls counts probes. A second set must reuse s.infer[id] and issue no further sweep.
func TestRealSetCachesInference(t *testing.T) {
	fh := startFakeHost(t)
	defer fh.stop()
	s := newLiveTestSession() // cutoff: linear 20..20000 Hz, so real= inverts analytically (no refine round-trips)
	ctx := context.Background()
	if _, _, err := s.handleConnectLive(ctx, nil, connectLiveIn{Host: "127.0.0.1", Port: fh.port()}); err != nil {
		t.Fatalf("connect_live: %v", err)
	}

	// First real set: probes once (one get_param opens the sweep).
	first := 1000.0
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Real: &first}); err != nil {
		t.Fatalf("first set real: %v", err)
	}
	fh.mu.Lock()
	afterFirst := fh.cmds["get_param"]
	fh.mu.Unlock()
	if afterFirst != 1 {
		t.Fatalf("first real set should probe exactly once (1 get_param), saw %d", afterFirst)
	}

	// Second real set on the SAME param: must reuse the cached inference, no new sweep.
	second := 5000.0
	if _, _, err := s.handleSetParam(ctx, nil, setParamIn{ID: "cutoff", Real: &second}); err != nil {
		t.Fatalf("second set real: %v", err)
	}
	// A batch set on the same param, too: still no re-probe.
	if _, _, err := s.handleSetParams(ctx, nil, setParamsIn{Params: []setParamsRow{{ID: "cutoff", Real: ptrF(8000)}}}); err != nil {
		t.Fatalf("batch set real: %v", err)
	}
	fh.mu.Lock()
	afterSecond := fh.cmds["get_param"]
	fh.mu.Unlock()
	if afterSecond != 1 {
		t.Fatalf("repeated real sets re-probed: get_param count went %d -> %d, want it to stay 1", afterFirst, afterSecond)
	}
}

// textOf pulls the first text content out of a tool result.
func textOf(r *mcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}
