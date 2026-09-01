// live_test.go - loopback test for the live-control path. Stands up a FAKE host listener (a goroutine speaking
// the ControlListener line-JSON protocol against an in-memory param map) and drives it through the session's
// live path: connect_live -> set_param forwards + the fake records it -> get_param reads it back -> play_note
// reaches the fake -> all_notes_off panics it -> disconnect_live returns to headless.
//
// This exercises the Go forwarder end to end without a running C++ host; the real cpp/ControlListener.h speaks
// the identical protocol, so this is "same bytes on the socket."

package sidechain

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeHost is a minimal stand-in for cpp/ControlListener: it accepts one client and answers the same commands
// over the same line-delimited JSON protocol, backed by an in-memory normalized-value map.
type fakeHost struct {
	ln     net.Listener
	mu     sync.Mutex
	params map[string]float64 // id -> normalized
	notes  []int              // notes turned on
	panics int                // all_notes_off count
}

func startFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake host listen: %v", err)
	}
	fh := &fakeHost{ln: ln, params: map[string]float64{}}
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
		resp := fh.dispatch(req)
		if id, ok := req["id"]; ok {
			resp["id"] = id
		}
		b, _ := json.Marshal(resp)
		conn.Write(append(b, '\n'))
	}
}

func (fh *fakeHost) dispatch(req map[string]any) map[string]any {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	cmd, _ := req["cmd"].(string)
	switch cmd {
	case "ping":
		return map[string]any{"ok": true, "pong": true}
	case "set_param":
		id, _ := req["param"].(string)
		norm, _ := req["normalized"].(float64)
		fh.params[id] = norm
		return map[string]any{"ok": true, "param": id, "normalized": norm, "value": norm, "text": "ok"}
	case "get_param":
		id, _ := req["param"].(string)
		norm := fh.params[id]
		return map[string]any{"ok": true, "param": id, "normalized": norm, "value": norm, "text": "ok"}
	case "note_on":
		n, _ := req["note"].(float64)
		fh.notes = append(fh.notes, int(n))
		return map[string]any{"ok": true, "note": int(n), "on": true}
	case "note_off":
		return map[string]any{"ok": true, "on": false}
	case "all_notes_off":
		fh.panics++
		return map[string]any{"ok": true}
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
