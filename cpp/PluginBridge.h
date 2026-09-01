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
};

} // namespace sidechain
