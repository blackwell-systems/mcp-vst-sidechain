#pragma once
#include <string>
#include <utility>
#include <vector>

// ================================================================================================
// sidechain::PluginBridge - the seam between the VST-agnostic control plane (ControlServer) and a concrete
// plugin. The control plane knows a plugin ONLY through this interface: it never sees juce::AudioProcessor,
// a parameter type, or a state format. Everything plugin-specific (a parameter's real<->normalized curve, its
// step count, its opaque state bytes, its MIDI target) lives behind an implementation of this interface
// (JucePluginBridge is the one that hosts a real VST3/AU). That is what lets the transport/protocol/identity/
// event machinery be reused for anything controllable, not just a JUCE plugin.
//
// THREADING CONTRACT (mirrors docs/CONCURRENCY.md, enforced by the ControlServer that drives this):
//   • READ-side methods (paramCount, hasParam, readParam, describeIndex, snapshotAll, resolveSet) are called
//     from MANY threads (socket handlers) and must be safe there. For a plugin they read parameter atomics.
//   • APPLY-side methods (applyParam, noteOn/noteOff/allNotesOff, resetInit, loadState, saveState) are called
//     ONLY on the host's single applier thread (the JUCE message thread), one at a time, so an implementation
//     needs no internal locking for them.
//   • The bridge reports parameter changes (from any source: a controller, the plugin's editor, host
//     automation) by calling the ParamChangeSink it was given. That callback may fire on ANY thread; the sink
//     (ControlServer) decides whether to broadcast immediately or defer, so the bridge just forwards the index.
// ================================================================================================

namespace sidechain
{

// A parameter's observable value in the three representations the wire protocol reports.
struct ParamValue
{
    double      value      = 0.0;   // catalog space: real units (continuous native), a step index (discrete), else normalized
    float       normalized = 0.0f;  // always 0..1
    std::string text;               // the plugin's own rendered value text
};

// How a set_param request expressed its target. The mapping to a normalized 0..1 is plugin-specific (step
// count, a native param's real<->normalized curve), so it is resolved behind the bridge, not in the control plane.
enum class SetForm { Normalized, Choice, Value };

// The excitation signal for an EFFECT render (an instrument is driven by a MIDI note instead). `Sine` is the
// default: a fixed test tone that is easy to reason about; `Noise` is a flat-ish excitation for filter/EQ work;
// `Impulse` a single unit sample; `Silence` no input (measures a self-oscillating or tail-only effect).
enum class InputKind { Silence, Sine, Noise, Impulse };

// A render request: what to feed the hosted plugin and how long to render. All fields carry the wire defaults.
// The host AUTO-DETECTS instrument (accepts MIDI, no audio input => driven by note/velocity/channel + gateMs) vs
// effect (has an audio input => driven by inputKind/inputFreq/inputLevel); the caller need not choose.
struct RenderSpec
{
    int   note     = 60;      // MIDI note for an instrument render (note-on at sample 0, note-off at gateMs)
    float velocity = 0.8f;    // note-on velocity, 0..1
    int   channel  = 1;       // MIDI channel, 1..16
    int   gateMs   = 500;     // how long the note is held (instrument); the tail is durationMs - gateMs
    int   durationMs = 2000;  // total render length in ms (instrument: gate + tail; effect: whole signal)

    InputKind inputKind = InputKind::Sine;   // excitation for an effect render
    double    inputFreq = 220.0;             // Sine frequency in Hz
    float     inputLevel = 0.5f;             // Sine / Noise / Impulse amplitude, 0..1
};

// The measurement returned from a render. The peak/rms/crest/silent/clipped half is the JUCE-free
// BasicMeasurement (RenderAnalysis.h, unit-tested standalone); centroid/bands come from the FFT path in the
// JUCE-linked bridge. `ok` is false when there is nothing to render (no plugin) or the render could not run.
struct Measurement
{
    bool   ok = false;

    double durationSec = 0.0;
    double sampleRate  = 0.0;
    int    channels    = 0;

    double peakDb = -160.0;
    double rmsDb  = -160.0;
    double crest  = 0.0;

    double centroidHz = 0.0;   // spectral centroid ("brightness"), Hz
    double lowDb  = -160.0;    // energy < 200 Hz, dBFS
    double midDb  = -160.0;    // energy 200 Hz .. 2 kHz, dBFS
    double highDb = -160.0;    // energy > 2 kHz, dBFS

    bool   silent  = true;
    bool   clipped = false;
};

// The control plane implements this so the bridge can report a parameter change. May be invoked from ANY thread
// (a plugin's listener callback carries no thread guarantee); the sink decides whether to broadcast now or defer.
class ParamChangeSink
{
public:
    virtual ~ParamChangeSink() = default;
    virtual void onParamChanged (int index) = 0;
};

class PluginBridge
{
public:
    virtual ~PluginBridge() = default;

    // Register (or clear, with nullptr) the sink the bridge notifies on a parameter change. The control plane
    // sets this to itself while attached and back to nullptr before it tears down.
    virtual void setChangeSink (ParamChangeSink* sink) = 0;

    // ---- read side (any thread) ----
    virtual int  paramCount() const = 0;

    // The plugin's distinct parameter groups, in first-seen order (empty for a flat plugin). These are the
    // leasable edit "sections" the C3 governed layer binds its section leases to (see ControlServer / GovernedState.h).
    virtual std::vector<std::string> sectionGroups() const = 0;
    virtual bool hasParam (const std::string& id) const = 0;
    virtual bool readParam (const std::string& id, ParamValue& out) const = 0;
    virtual bool describeIndex (int index, std::string& idOut, ParamValue& out) const = 0;
    virtual std::vector<std::pair<std::string, ParamValue>> snapshotAll() const = 0;

    // Resolve a set request (a form + a raw number) to a parameter index and a normalized 0..1 target. Returns
    // false only when the id is unknown; clamping and the curve are the implementation's job.
    virtual bool resolveSet (const std::string& id, SetForm form, double v, int& indexOut, float& normOut) const = 0;

    // ---- apply side (the single applier thread only) ----
    virtual void applyParam (int index, float norm) = 0;
    virtual void noteOn  (int chan, int note, float vel) = 0;
    virtual void noteOff (int chan, int note, float vel) = 0;
    virtual void allNotesOff() = 0;
    virtual void resetInit() = 0;                          // host-supplied init/default patch; a no-op ack if unwired
    virtual bool loadState (const std::string& base64) = 0;  // whole-patch recall; false on failure
    virtual bool saveState (std::string& base64Out) = 0;     // whole-patch snapshot; false if unavailable

    // Offline-render the plugin per the spec and measure the accumulated output. Runs on the single applier
    // thread (like get_full_state): it re-prepares the processor (resets DSP state, not params) so the render is
    // deterministic, drives it with a MIDI note (instrument) or a synthesized input signal (effect), loops
    // processBlock over the duration, and analyzes the output. Returns a Measurement with ok=false if there is
    // nothing to render. The default is a no-op ok=false so a non-audio bridge need not implement it.
    virtual Measurement renderAndMeasure (const RenderSpec&) { return {}; }
};

} // namespace sidechain
