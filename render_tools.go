// render_tools.go - the render_and_measure MCP tool: the agent's EARS. It drives an offline render of the
// running plugin (a MIDI note through an instrument, or a synthesized test signal through an effect; the host
// auto-detects) and returns an objective measurement of the output - level, dynamics, brightness, silence, clip -
// so the agent can EVALUATE its own edits ("did it get brighter?") instead of tweaking blind. Live only: it needs
// the running instance to render. See docs/RENDER-SCOPING.md (Tier 1 render + Tier 2 analysis).

package sidechain

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type renderAndMeasureIn struct {
	Note       int     `json:"note,omitempty" jsonschema:"MIDI note for an instrument render (0..127; 60 = middle C). Ignored for an effect. Host default if omitted."`
	Velocity   float64 `json:"velocity,omitempty" jsonschema:"note velocity 0..1 (instrument). Host default if omitted."`
	Channel    int     `json:"channel,omitempty" jsonschema:"MIDI channel 1..16 (instrument). Host default if omitted."`
	GateMs     int     `json:"gateMs,omitempty" jsonschema:"note-on..note-off gate in ms (instrument): how long the note is held before the release tail. Host default if omitted."`
	DurationMs int     `json:"durationMs,omitempty" jsonschema:"total render length in ms (gate + release tail). Host default if omitted."`
	InputKind  string  `json:"inputKind,omitempty" jsonschema:"effect excitation signal: sine | noise | impulse | silence. Ignored for an instrument. Host default (sine) if omitted."`
	InputFreq  float64 `json:"inputFreq,omitempty" jsonschema:"sine frequency in Hz when inputKind=sine (effect)."`
	InputLevel float64 `json:"inputLevel,omitempty" jsonschema:"input signal level 0..1 (effect)."`
}

// renderResult is the structured payload: the full measurement object plus the one-line human summary that the
// tool also returns as text.
type renderResult struct {
	Measurement Measurement `json:"measurement"`
	Summary     string      `json:"summary"`
}

// renderSummary formats the headline measurement as a single human line, e.g.
// "peak -6.2 dBFS, RMS -18.4 dB, centroid 1.84 kHz, not clipped".
func renderSummary(m Measurement) string {
	clip := "not clipped"
	if m.Clipped {
		clip = "CLIPPED"
	}
	if m.Silent {
		return fmt.Sprintf("silent (peak %.1f dBFS, RMS %.1f dB): no output - dead patch?", m.PeakDb, m.RmsDb)
	}
	return fmt.Sprintf("peak %.1f dBFS, RMS %.1f dB, centroid %.2f kHz, %s",
		m.PeakDb, m.RmsDb, m.CentroidHz/1000.0, clip)
}

func (s *session) handleRenderAndMeasure(ctx context.Context, _ *mcp.CallToolRequest, in renderAndMeasureIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	s.mu.Unlock()
	if lc == nil {
		return textResult("render_and_measure: not live. Call connect_live first (rendering needs the running plugin)."), nil, nil
	}
	// renderAndMeasureIn and RenderSpec have identical fields (the JSON tags differ, which conversion ignores),
	// so the input converts directly to the wire spec.
	m, err := lc.Render(RenderSpec(in))
	if err != nil {
		return textResult("render_and_measure failed: " + err.Error()), nil, nil
	}
	summary := renderSummary(m)
	return textResult(summary), renderResult{Measurement: m, Summary: summary}, nil
}

// registerRenderTools wires the render_and_measure tool. Called from NewServer alongside the other tool sets.
func registerRenderTools(srv *mcp.Server, s *session) {
	mcp.AddTool(srv, &mcp.Tool{Name: "render_and_measure", Description: "Render the running plugin OFFLINE (a MIDI note for an instrument, or a test signal for an effect; auto-detected) and return an objective measurement of the output: peak/RMS/crest levels, spectral centroid (brightness), a 3-band energy split, and silent/clipped flags. Use it to EVALUATE an edit ('did it get brighter?') instead of guessing. Live only."}, s.handleRenderAndMeasure)
}
