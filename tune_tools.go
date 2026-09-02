// tune_tools.go - Phase 4, increment 1: the tune_param closed-loop optimizer. It drives ONE parameter toward a
// goal on ONE measurement, using an offline render at each step as the objective function. The AGENT supplies the
// (param, measure, direction) triple from its reasoning over the semantic map (Phase 3); this tool converges the
// param objectively and returns the trace. The bridge holds no intent ontology: "brighter" -> (cutoff, centroid_hz,
// maximize) is the agent's call, the search is ours. Live only (it renders). See docs/PHASE4-SCOPING.md.

package sidechain

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type tuneParamIn struct {
	ID      string  `json:"id" jsonschema:"the plugin parameter id to tune (a continuous param; use set_param choice= for discrete lists)"`
	Measure string  `json:"measure" jsonschema:"the objective measurement: centroid_hz | peak_db | rms_db | crest | low_db | mid_db | high_db, or a nested modulation measure: modulation.centroid.rate_hz | modulation.centroid.depth | modulation.rms.rate_hz | modulation.rms.depth"`
	Goal    string  `json:"goal" jsonschema:"maximize | minimize | target"`
	Target  float64 `json:"target,omitempty" jsonschema:"the target value in the measure's unit; required when goal=target (e.g. rms_db target -12)"`
	Seeds   int     `json:"seeds,omitempty" jsonschema:"coarse uniform samples across the normalized range (default 5, min 2)"`
	Refine  int     `json:"refineIters,omitempty" jsonschema:"golden-section refinement steps after the coarse pass (default 4)"`
	Restore bool    `json:"restore,omitempty" jsonschema:"if true, restore the param to its starting value after searching (measure-only what-if); default false leaves it at the best value found"`

	// Render fields, held FIXED across the whole search so only the tuned param varies. Identical to
	// render_and_measure; host defaults apply when omitted.
	Note       int     `json:"note,omitempty" jsonschema:"MIDI note for an instrument render (0..127; 60 = middle C)."`
	Velocity   float64 `json:"velocity,omitempty" jsonschema:"note velocity 0..1 (instrument)."`
	Channel    int     `json:"channel,omitempty" jsonschema:"MIDI channel 1..16 (instrument)."`
	GateMs     int     `json:"gateMs,omitempty" jsonschema:"note-on..note-off gate in ms (instrument)."`
	DurationMs int     `json:"durationMs,omitempty" jsonschema:"total render length in ms."`
	InputKind  string  `json:"inputKind,omitempty" jsonschema:"effect excitation: sine | noise | impulse | silence."`
	InputFreq  float64 `json:"inputFreq,omitempty" jsonschema:"sine frequency in Hz when inputKind=sine (effect)."`
	InputLevel float64 `json:"inputLevel,omitempty" jsonschema:"input signal level 0..1 (effect)."`
}

// tuneEval is one point in the search trace: a normalized position and the measure's value there.
type tuneEval struct {
	Normalized float64 `json:"normalized"`
	Value      float64 `json:"value"`
}

// tuneResult is the structured payload: the starting and best positions, the full evaluation trace, and the
// complete measurement at the best position.
type tuneResult struct {
	ID              string      `json:"id"`
	Measure         string      `json:"measure"`
	Goal            string      `json:"goal"`
	Target          *float64    `json:"target,omitempty"`
	StartNormalized float64     `json:"startNormalized"`
	StartValue      float64     `json:"startValue"`
	BestNormalized  float64     `json:"bestNormalized"`
	BestValue       float64     `json:"bestValue"`
	Restored        bool        `json:"restored"`
	Evaluations     []tuneEval  `json:"evaluations"`
	Measurement     Measurement `json:"measurement"`
	Summary         string      `json:"summary"`
}

// measureValue pulls one named scalar out of a Measurement. The name matches the snake_case wire vocabulary so the
// agent names the same measure it sees in render_and_measure output. Nested modulation measures
// (modulation.centroid.rate_hz, modulation.centroid.depth, modulation.rms.rate_hz, modulation.rms.depth) read from
// m.Modulation and return ok=false when the Modulation block is nil (temporal was not requested or not returned).
// Both snake_case (rate_hz) and camelCase (rateHz) variants are accepted for friendliness.
func measureValue(m Measurement, measure string) (float64, bool) {
	key := strings.ToLower(strings.TrimSpace(measure))
	switch key {
	case "centroid_hz", "centroidhz", "centroid":
		return m.CentroidHz, true
	case "peak_db", "peakdb", "peak":
		return m.PeakDb, true
	case "rms_db", "rmsdb", "rms":
		return m.RmsDb, true
	case "crest":
		return m.Crest, true
	case "low_db", "lowdb", "low":
		return m.Bands.LowDb, true
	case "mid_db", "middb", "mid":
		return m.Bands.MidDb, true
	case "high_db", "highdb", "high":
		return m.Bands.HighDb, true
	// Tier 2.5 nested modulation measures. Accept snake_case and camelCase for the leaf field.
	case "modulation.centroid.rate_hz", "modulation.centroid.ratehz":
		if m.Modulation == nil {
			return 0, false
		}
		return m.Modulation.Centroid.RateHz, true
	case "modulation.centroid.depth":
		if m.Modulation == nil {
			return 0, false
		}
		return m.Modulation.Centroid.Depth, true
	case "modulation.rms.rate_hz", "modulation.rms.ratehz":
		if m.Modulation == nil {
			return 0, false
		}
		return m.Modulation.Rms.RateHz, true
	case "modulation.rms.depth":
		if m.Modulation == nil {
			return 0, false
		}
		return m.Modulation.Rms.Depth, true
	}
	return 0, false
}

// isModulationMeasure reports whether the named measure requires a Tier 2.5 temporal analysis block (i.e. whether
// the render spec must have Temporal=true). Any measure that starts with "modulation." qualifies.
func isModulationMeasure(measure string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(measure)), "modulation.")
}

// tuneScore turns a measure value into a "higher is better" score for the goal, so the search always maximizes.
func tuneScore(value float64, goal string, target float64) float64 {
	switch goal {
	case "minimize":
		return -value
	case "target":
		return -math.Abs(value - target)
	default: // maximize
		return value
	}
}

func (s *session) handleTuneParam(ctx context.Context, _ *mcp.CallToolRequest, in tuneParamIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	p := s.catalog.Get(in.ID)
	s.mu.Unlock()

	if lc == nil {
		return textResult("tune_param: not live. Call connect_live first (tuning renders the running plugin)."), nil, nil
	}
	if p == nil {
		return textResult(fmt.Sprintf("tune_param: unknown id %q. Use list_params to discover ids.", in.ID)), nil, nil
	}
	if p.Type == "choice" {
		return textResult(fmt.Sprintf("tune_param: %q is a choice param, not a continuum. Use set_param choice=<name> and render_and_measure to compare.", in.ID)), nil, nil
	}
	if _, ok := measureValue(Measurement{Modulation: &Modulation{}}, in.Measure); !ok {
		return textResult(fmt.Sprintf("tune_param: unknown measure %q. Use one of centroid_hz, peak_db, rms_db, crest, low_db, mid_db, high_db, or modulation.centroid.rate_hz / modulation.centroid.depth / modulation.rms.rate_hz / modulation.rms.depth.", in.Measure)), nil, nil
	}
	goal := strings.ToLower(strings.TrimSpace(in.Goal))
	switch goal {
	case "maximize", "minimize", "target":
	default:
		return textResult(fmt.Sprintf("tune_param: unknown goal %q. Use maximize, minimize, or target.", in.Goal)), nil, nil
	}

	seeds := in.Seeds
	if seeds < 2 {
		seeds = 5
	}
	refine := in.Refine
	if in.Refine == 0 {
		refine = 4
	}
	if refine < 0 {
		refine = 0
	}

	spec := RenderSpec{
		Note: in.Note, Velocity: in.Velocity, Channel: in.Channel, GateMs: in.GateMs,
		DurationMs: in.DurationMs, InputKind: in.InputKind, InputFreq: in.InputFreq, InputLevel: in.InputLevel,
	}
	// Modulation measures require temporal analysis. Auto-enable it so the host returns a modulation block; a
	// sensible default frame size (25 ms) is left as zero so the host applies its own default.
	if isModulationMeasure(in.Measure) {
		spec.Temporal = true
	}

	// The starting position, so we can restore it and report the delta.
	_, startNorm, _, err := lc.GetParam(p.ID)
	if err != nil {
		return textResult("tune_param: could not read the current value: " + err.Error()), nil, nil
	}

	// renderAt sets the param to a normalized position and renders, returning the measure's value there plus the
	// full measurement. Every call is recorded in the trace, and the running best (by score) is tracked.
	var evals []tuneEval
	var bestNorm, bestVal, bestScore float64
	var bestMeas Measurement
	haveBest := false
	renderAt := func(norm float64) (float64, error) {
		norm = math.Max(0, math.Min(1, norm))
		if _, _, _, e := lc.SetParam(p.ID, norm, false); e != nil {
			return 0, e
		}
		m, e := lc.Render(spec)
		if e != nil {
			return 0, e
		}
		// Guard: a modulation measure requires the host to have returned a modulation block. When the block is
		// absent (the plugin/host does not support temporal analysis, or Temporal was not set), fail early with
		// an actionable message rather than silently scoring as zero.
		if isModulationMeasure(in.Measure) && m.Modulation == nil {
			return 0, fmt.Errorf("host returned no modulation block; the plugin/host may not support temporal analysis")
		}
		v, _ := measureValue(m, in.Measure)
		evals = append(evals, tuneEval{Normalized: norm, Value: v})
		sc := tuneScore(v, goal, in.Target)
		if !haveBest || sc > bestScore {
			haveBest, bestScore, bestNorm, bestVal, bestMeas = true, sc, norm, v, m
		}
		return v, nil
	}

	// Baseline at the starting position (so startValue reflects the patch as found, and it competes as a candidate).
	startVal, err := renderAt(startNorm)
	if err != nil {
		return textResult("tune_param: baseline render failed: " + err.Error()), nil, nil
	}

	// Coarse pass: uniform seeds across [0,1] inclusive. Find the best, then bracket it with its neighbors.
	for i := 0; i < seeds; i++ {
		x := float64(i) / float64(seeds-1)
		if _, err := renderAt(x); err != nil {
			return textResult("tune_param: render failed during coarse pass: " + err.Error()), nil, nil
		}
	}
	step := 1.0 / float64(seeds-1)
	a := math.Max(0, bestNorm-step)
	b := math.Min(1, bestNorm+step)

	// Golden-section refine within the bracket. Standard 0.618 shrink; one new render per iteration after the two
	// interior seeds. Reuses the score already collected via renderAt's running best.
	if b-a > 1e-6 && refine > 0 {
		const gr = 0.6180339887498949
		c := b - gr*(b-a)
		d := a + gr*(b-a)
		fc, err := renderAt(c)
		if err != nil {
			return textResult("tune_param: refine render failed: " + err.Error()), nil, nil
		}
		fd, err := renderAt(d)
		if err != nil {
			return textResult("tune_param: refine render failed: " + err.Error()), nil, nil
		}
		scoreOf := func(v float64) float64 { return tuneScore(v, goal, in.Target) }
		for i := 0; i < refine; i++ {
			if scoreOf(fc) > scoreOf(fd) {
				b, d, fd = d, c, fc
				c = b - gr*(b-a)
				if fc, err = renderAt(c); err != nil {
					return textResult("tune_param: refine render failed: " + err.Error()), nil, nil
				}
			} else {
				a, c, fc = c, d, fd
				d = a + gr*(b-a)
				if fd, err = renderAt(d); err != nil {
					return textResult("tune_param: refine render failed: " + err.Error()), nil, nil
				}
			}
		}
	}

	// Land the param: at the best value found, or restored to the start for a measure-only what-if.
	landNorm := bestNorm
	if in.Restore {
		landNorm = startNorm
	}
	if _, _, _, e := lc.SetParam(p.ID, landNorm, false); e != nil {
		return textResult("tune_param: failed to set the final value: " + e.Error()), nil, nil
	}

	// Sort the trace by position for a readable curve (the search order is not monotonic).
	sort.SliceStable(evals, func(i, j int) bool { return evals[i].Normalized < evals[j].Normalized })

	var tgt *float64
	if goal == "target" {
		t := in.Target
		tgt = &t
	}
	landDesc := fmt.Sprintf("left at normalized %.3f", bestNorm)
	if in.Restore {
		landDesc = fmt.Sprintf("restored to normalized %.3f (best was %.3f)", startNorm, bestNorm)
	}
	summary := fmt.Sprintf("tuned %s: %s %s -> %s over %d renders (%s)",
		p.Label, in.Measure, formatMeasure(in.Measure, startVal), formatMeasure(in.Measure, bestVal), len(evals), landDesc)

	out := tuneResult{
		ID: p.ID, Measure: in.Measure, Goal: goal, Target: tgt,
		StartNormalized: startNorm, StartValue: startVal,
		BestNormalized: bestNorm, BestValue: bestVal, Restored: in.Restore,
		Evaluations: evals, Measurement: bestMeas, Summary: summary,
	}
	return textResult(summary), out, nil
}

// formatMeasure renders a measure value with a unit hint for the human summary line: Hz for the centroid and for
// any modulation rate (a couple of decimals, since LFO rates are small), Hz for the centroid modulation depth, dB
// for levels/bands and the rms modulation depth, a bare number for crest.
func formatMeasure(measure string, v float64) string {
	key := strings.ToLower(strings.TrimSpace(measure))
	switch {
	case strings.Contains(key, "rate_hz") || strings.Contains(key, "ratehz"):
		return fmt.Sprintf("%.2f Hz", v) // LFO / modulation rate
	case key == "centroid_hz" || key == "centroidhz" || key == "centroid" || key == "modulation.centroid.depth":
		return fmt.Sprintf("%.0f Hz", v) // spectral centroid, or centroid modulation depth (Hz excursion)
	case key == "crest":
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.1f dB", v) // peak/rms/bands, or rms modulation depth (dB excursion)
	}
}

// registerTuneTools wires the tune_param tool. Called from NewServer alongside the other tool sets.
func registerTuneTools(srv *mcp.Server, s *session) {
	mcp.AddTool(srv, &mcp.Tool{Name: "tune_param", Description: "Drive ONE parameter toward a goal on ONE measurement, rendering + measuring at each step (a bounded closed-loop search). You pick the param, the measure (centroid_hz/peak_db/rms_db/crest/band), and the goal (maximize/minimize/target); the tool converges it and returns the value it settled on plus the search trace. This is the autonomous 'make it brighter' loop: pick the filter cutoff, measure=centroid_hz, goal=maximize. Live only."}, s.handleTuneParam)
}
