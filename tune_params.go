// tune_params.go - Phase 4 increment 2: tune_params, multi-parameter co-optimization by COORDINATE DESCENT. Where
// tune_param drives one param toward one measure, tune_params drives a SET of param->objective knobs together, for
// compositional intents the agent decomposes over the semantic map: "punchier" = attack + drive both toward higher
// crest; "wobble at 3 Hz, deep" = the LFO rate toward 3 Hz AND the LFO amount toward higher modulation depth (after
// the agent has set the LFO destination/waveform via set_param). Each round tunes every knob's param in turn with
// the shared 1-D search (tuneAxis), holding the others at their current best; rounds repeat until a round moves
// nothing (converged) or the round budget is spent. The bridge holds no intent ontology: the agent picks the knobs
// and their measures/directions; the server co-optimizes them. Live only (it renders). See docs/PHASE4-SCOPING.md.

package sidechain

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tuneKnob is one param->objective pair in a co-optimization: drive ID toward Goal on Measure (Target used when
// Goal=="target"). Same measure vocabulary as tune_param, including the nested modulation measures.
type tuneKnob struct {
	ID      string  `json:"id" jsonschema:"the plugin parameter id to tune (a continuous param)"`
	Measure string  `json:"measure" jsonschema:"the objective for THIS param: centroid_hz | peak_db | rms_db | crest | low_db | mid_db | high_db | modulation.centroid.rate_hz | modulation.centroid.depth | modulation.rms.rate_hz | modulation.rms.depth"`
	Goal    string  `json:"goal" jsonschema:"maximize | minimize | target"`
	Target  float64 `json:"target,omitempty" jsonschema:"target value in the measure's unit; required when goal=target"`
}

type tuneParamsIn struct {
	Knobs   []tuneKnob `json:"knobs" jsonschema:"the parameters to co-optimize, each with its own measure/goal/target. Two or more knobs are typical (e.g. attack + drive for 'punchier')."`
	Rounds  int        `json:"rounds,omitempty" jsonschema:"max coordinate-descent rounds (default 3); stops early when a whole round moves nothing"`
	Seeds   int        `json:"seeds,omitempty" jsonschema:"coarse uniform samples per 1-D axis (default 4, min 2). Kept small because many axes x rounds are rendered."`
	Refine  int        `json:"refineIters,omitempty" jsonschema:"golden-section refine steps per axis (default 3)"`
	Restore bool       `json:"restore,omitempty" jsonschema:"restore ALL knobs to their starting values after searching (a measure-only what-if); default false leaves each at its best."`

	// Render fields, held FIXED across the whole search so only the tuned params vary. Identical to render_and_measure.
	Note       int     `json:"note,omitempty" jsonschema:"MIDI note for an instrument render (0..127; 60 = middle C)."`
	Velocity   float64 `json:"velocity,omitempty" jsonschema:"note velocity 0..1 (instrument)."`
	Channel    int     `json:"channel,omitempty" jsonschema:"MIDI channel 1..16 (instrument)."`
	GateMs     int     `json:"gateMs,omitempty" jsonschema:"note-on..note-off gate in ms (instrument)."`
	DurationMs int     `json:"durationMs,omitempty" jsonschema:"total render length in ms."`
	InputKind  string  `json:"inputKind,omitempty" jsonschema:"effect excitation: sine | noise | impulse | silence."`
	InputFreq  float64 `json:"inputFreq,omitempty" jsonschema:"sine frequency in Hz when inputKind=sine (effect)."`
	InputLevel float64 `json:"inputLevel,omitempty" jsonschema:"input signal level 0..1 (effect)."`
}

// tuneKnobResult is the per-param outcome of the co-optimization.
type tuneKnobResult struct {
	ID              string   `json:"id"`
	Measure         string   `json:"measure"`
	Goal            string   `json:"goal"`
	Target          *float64 `json:"target,omitempty"`
	StartNormalized float64  `json:"startNormalized"`
	StartValue      float64  `json:"startValue"`
	BestNormalized  float64  `json:"bestNormalized"`
	BestValue       float64  `json:"bestValue"`
}

// tuneParamsResult is the structured payload: the per-knob outcomes plus how many rounds/renders it took.
type tuneParamsResult struct {
	Knobs    []tuneKnobResult `json:"knobs"`
	Rounds   int              `json:"rounds"`
	Renders  int              `json:"renders"`
	Restored bool             `json:"restored"`
	Summary  string           `json:"summary"`
}

// convergedEps is the normalized movement below which a knob is considered "not moved" this round. When every knob
// moves less than this in a round, the descent has converged and stops early.
const convergedEps = 0.02

func (s *session) handleTuneParams(ctx context.Context, _ *mcp.CallToolRequest, in tuneParamsIn) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	lc := s.live
	s.mu.Unlock()
	if lc == nil {
		return textResult("tune_params: not live. Call connect_live first (tuning renders the running plugin)."), nil, nil
	}
	if len(in.Knobs) == 0 {
		return textResult("tune_params: no knobs. Provide at least one {id, measure, goal[, target]} in knobs."), nil, nil
	}

	// Resolve + validate every knob up front (so a bad knob fails before any rendering).
	type axis struct {
		knob tuneKnob
		p    *ParamDef
		goal string
	}
	axes := make([]axis, 0, len(in.Knobs))
	anyModulation := false
	for i, k := range in.Knobs {
		p := s.catalog.Get(k.ID)
		if p == nil {
			return textResult(fmt.Sprintf("tune_params: knob %d: unknown id %q. Use list_params to discover ids.", i, k.ID)), nil, nil
		}
		if p.Type == "choice" {
			return textResult(fmt.Sprintf("tune_params: knob %d (%q) is a choice param, not a continuum. Set it with set_param choice= and leave it out of tune_params.", i, k.ID)), nil, nil
		}
		if _, ok := measureValue(Measurement{Modulation: &Modulation{}}, k.Measure); !ok {
			return textResult(fmt.Sprintf("tune_params: knob %d: unknown measure %q.", i, k.Measure)), nil, nil
		}
		goal := strings.ToLower(strings.TrimSpace(k.Goal))
		switch goal {
		case "maximize", "minimize", "target":
		default:
			return textResult(fmt.Sprintf("tune_params: knob %d: unknown goal %q. Use maximize, minimize, or target.", i, k.Goal)), nil, nil
		}
		if isModulationMeasure(k.Measure) {
			anyModulation = true
		}
		axes = append(axes, axis{knob: k, p: p, goal: goal})
	}

	rounds := in.Rounds
	if rounds < 1 {
		rounds = 3
	}
	seeds := in.Seeds
	if seeds < 2 {
		seeds = 4
	}
	refine := in.Refine
	if in.Refine == 0 {
		refine = 3
	}
	if refine < 0 {
		refine = 0
	}

	spec := RenderSpec{
		Note: in.Note, Velocity: in.Velocity, Channel: in.Channel, GateMs: in.GateMs,
		DurationMs: in.DurationMs, InputKind: in.InputKind, InputFreq: in.InputFreq, InputLevel: in.InputLevel,
	}
	// If any knob targets a modulation measure, the render must produce a modulation block for the whole search.
	if anyModulation {
		spec.Temporal = true
	}

	// Capture the true joint starting state: each knob's starting normalized position, and one render at the origin
	// so every knob's start VALUE is measured against the same untouched patch.
	startNorm := make([]float64, len(axes))
	for i, ax := range axes {
		_, n, _, err := lc.GetParam(ax.p.ID)
		if err != nil {
			return textResult("tune_params: could not read " + ax.p.ID + ": " + err.Error()), nil, nil
		}
		startNorm[i] = n
	}
	m0, err := lc.Render(spec)
	if err != nil {
		return textResult("tune_params: baseline render failed: " + err.Error()), nil, nil
	}
	if anyModulation && m0.Modulation == nil {
		return textResult("tune_params: host returned no modulation block; the plugin/host may not support temporal analysis."), nil, nil
	}
	startVal := make([]float64, len(axes))
	for i, ax := range axes {
		startVal[i], _ = measureValue(m0, ax.knob.Measure)
	}

	// Coordinate descent: each round, tune every knob in turn (holding the others at their current best). Stop when
	// a whole round moves nothing.
	bestNorm := make([]float64, len(axes))
	bestVal := make([]float64, len(axes))
	copy(bestNorm, startNorm)
	totalRenders := 1 // the baseline render above
	roundsRun := 0
	for r := 0; r < rounds; r++ {
		roundsRun++
		moved := false
		for i, ax := range axes {
			ao, err := tuneAxis(lc, axisSpec{p: ax.p, measure: ax.knob.Measure, goal: ax.goal, target: ax.knob.Target, seeds: seeds, refine: refine}, spec)
			if err != nil {
				return textResult(fmt.Sprintf("tune_params: knob %d (%s): %v", i, ax.p.Label, err)), nil, nil
			}
			totalRenders += len(ao.evals)
			if abs(ao.bestNorm-bestNorm[i]) > convergedEps {
				moved = true
			}
			bestNorm[i], bestVal[i] = ao.bestNorm, ao.bestVal
		}
		if !moved {
			break
		}
	}

	// Restore all knobs to their starting values for a measure-only what-if (otherwise each is already at its best).
	if in.Restore {
		for i, ax := range axes {
			if _, _, _, e := lc.SetParam(ax.p.ID, startNorm[i], false); e != nil {
				return textResult("tune_params: failed to restore " + ax.p.ID + ": " + e.Error()), nil, nil
			}
		}
	}

	// Build the per-knob results and a compact one-line summary.
	knobResults := make([]tuneKnobResult, len(axes))
	phrases := make([]string, len(axes))
	for i, ax := range axes {
		var tgt *float64
		if ax.goal == "target" {
			t := ax.knob.Target
			tgt = &t
		}
		knobResults[i] = tuneKnobResult{
			ID: ax.p.ID, Measure: ax.knob.Measure, Goal: ax.goal, Target: tgt,
			StartNormalized: startNorm[i], StartValue: startVal[i],
			BestNormalized: bestNorm[i], BestValue: bestVal[i],
		}
		phrases[i] = fmt.Sprintf("%s %s %s->%s", ax.p.Label, ax.knob.Measure,
			formatMeasure(ax.knob.Measure, startVal[i]), formatMeasure(ax.knob.Measure, bestVal[i]))
	}
	land := "left at best"
	if in.Restore {
		land = "restored to start"
	}
	summary := fmt.Sprintf("co-tuned %d params over %d round(s), %d renders (%s): %s",
		len(axes), roundsRun, totalRenders, land, strings.Join(phrases, "; "))

	return textResult(summary), tuneParamsResult{
		Knobs: knobResults, Rounds: roundsRun, Renders: totalRenders, Restored: in.Restore, Summary: summary,
	}, nil
}

// abs is a small float helper (avoids importing math for one call in this file).
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// registerTuneParamsTools wires the tune_params tool. Called from NewServer alongside the other tool sets.
func registerTuneParamsTools(srv *mcp.Server, s *session) {
	mcp.AddTool(srv, &mcp.Tool{Name: "tune_params", Description: "Co-optimize SEVERAL params at once toward their own goals, rendering + measuring at each step (coordinate descent over the render loop). Give a list of knobs, each {id, measure, goal[, target]}; the tool tunes them together and reports where each landed. Use for compositional intents one param can't express: 'punchier' = attack + drive toward higher crest; 'wobble' = the LFO rate toward a target AND the LFO amount toward more depth (set the LFO destination/waveform with set_param first). Live only."}, s.handleTuneParams)
}
