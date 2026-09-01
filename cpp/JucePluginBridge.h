#pragma once
#include <juce_audio_processors/juce_audio_processors.h>
#include <juce_audio_basics/juce_audio_basics.h>
#include <atomic>
#include <cmath>
#include <functional>
#include <memory>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>
#include "PluginBridge.h"

// ================================================================================================
// sidechain::JucePluginBridge - the concrete PluginBridge that hosts a real VST3/AU. It is the ONLY piece that
// touches juce::AudioProcessor: it maps parameter ids to the plugin's parameters, converts between normalized
// 0..1 and catalog space (real units for a continuous native param via its NormalisableRange, a step index for
// a discrete one), reads/writes parameter values, round-trips the plugin's opaque state as base64, plays notes
// through a MidiKeyboardState, and reports every parameter change to the control plane by registering as a
// juce::AudioProcessorParameter::Listener. Everything transport/protocol/identity/event lives in ControlServer,
// which knows this class only through the PluginBridge interface.
//
// Threading: read-side methods snapshot parameter atomics and are safe from any thread; apply-side methods are
// called only on the message thread by ControlServer (see PluginBridge.h and docs/CONCURRENCY.md).
// ================================================================================================

namespace sidechain
{

class JucePluginBridge : public PluginBridge,
                         private juce::AudioProcessorParameter::Listener
{
public:
    JucePluginBridge (juce::AudioProcessor& proc, juce::MidiKeyboardState& kbd)
        : processor (proc), keyboardState (kbd)
    {
        // Build the id -> param map ONCE on the BASE AudioProcessorParameter API (a hosted VST3/AU exposes
        // HostedAudioProcessorParameter, not RangedAudioParameter). Also build an index -> ParamRef view for the
        // change-event path, and register as a per-parameter listener so we hear ALL changes.
        const auto& params = processor.getParameters();
        byIndex.assign ((size_t) params.size(), nullptr);
        for (int i = 0; i < params.size(); ++i)
        {
            auto* p = params[i];
            std::string id = std::to_string (i);
            if (auto* hp = dynamic_cast<juce::HostedAudioProcessorParameter*> (p))
                if (hp->getParameterID().isNotEmpty())
                    id = hp->getParameterID().toStdString();
            auto* ranged = dynamic_cast<juce::RangedAudioParameter*> (p);
            byId[id] = ParamRef { i, p, ranged, ranged != nullptr && ! p->isDiscrete(), id };
            byIndex[(size_t) i] = &byId[id];   // node-based map => pointer stays valid as more are inserted
        }
        for (auto* p : params) p->addListener (this);
    }

    ~JucePluginBridge() override
    {
        for (auto* p : processor.getParameters()) p->removeListener (this);
    }

    // Optional message-thread action for reset_init. Null => a no-op ack. Kept as a callback so a host can wire
    // its own init/default patch without this class depending on one.
    std::function<void()> onResetInit;

    // ---- PluginBridge: change sink ----
    void setChangeSink (ParamChangeSink* s) override { sink.store (s); }

    // ---- PluginBridge: read side (any thread) ----
    int  paramCount() const override { return (int) byIndex.size(); }

    bool hasParam (const std::string& id) const override { return byId.find (id) != byId.end(); }

    bool readParam (const std::string& id, ParamValue& out) const override
    {
        auto it = byId.find (id);
        if (it == byId.end())
            return false;
        fill (it->second, out, true);
        return true;
    }

    bool describeIndex (int index, std::string& idOut, ParamValue& out) const override
    {
        if (index < 0 || index >= (int) byIndex.size() || byIndex[(size_t) index] == nullptr)
            return false;
        const ParamRef& r = *byIndex[(size_t) index];
        idOut = r.id;
        fill (r, out, true);
        return true;
    }

    std::vector<std::pair<std::string, ParamValue>> snapshotAll() const override
    {
        std::vector<std::pair<std::string, ParamValue>> out;
        out.reserve (byId.size());
        for (const auto& kv : byId)
        {
            ParamValue pv;
            fill (kv.second, pv, false);   // value + normalized; text omitted (get_state does not report it)
            out.emplace_back (kv.first, pv);
        }
        return out;
    }

    bool resolveSet (const std::string& id, SetForm form, double v, int& indexOut, float& normOut) const override
    {
        auto it = byId.find (id);
        if (it == byId.end())
            return false;
        auto* rp = it->second.rp;
        const int steps = rp->getNumSteps();
        float norm;
        switch (form)
        {
            case SetForm::Normalized:
                norm = juce::jlimit (0.0f, 1.0f, (float) v);
                break;
            case SetForm::Choice:
            {
                const int idx = (int) v;
                norm = (steps > 1) ? juce::jlimit (0.0f, 1.0f, (float) idx / (float) (steps - 1)) : 0.0f;
                break;
            }
            case SetForm::Value:
            default:
                if (it->second.useReal)
                    norm = juce::jlimit (0.0f, 1.0f, it->second.ranged->convertTo0to1 ((float) v));
                else
                    norm = (steps > 1) ? juce::jlimit (0.0f, 1.0f, (float) v / (float) (steps - 1))
                                       : juce::jlimit (0.0f, 1.0f, (float) v);
                break;
        }
        indexOut = it->second.index;
        normOut  = norm;
        return true;
    }

    // ---- PluginBridge: apply side (message thread only) ----
    void applyParam (int index, float norm) override
    {
        if (auto* p = processor.getParameters()[index])
            p->setValueNotifyingHost (norm);
    }
    void noteOn  (int chan, int note, float vel) override { keyboardState.noteOn  (chan, note, vel); }
    void noteOff (int chan, int note, float vel) override { keyboardState.noteOff (chan, note, vel); }
    void allNotesOff() override
    {
        for (int ch = 1; ch <= 16; ++ch) keyboardState.allNotesOff (ch);
    }
    void resetInit() override { if (onResetInit) onResetInit(); }

    bool loadState (const std::string& base64) override
    {
        juce::MemoryOutputStream decoded;
        if (juce::Base64::convertFromBase64 (decoded, juce::String (base64)) && decoded.getDataSize() > 0)
        {
            processor.setStateInformation (decoded.getData(), (int) decoded.getDataSize());
            return true;
        }
        return false;
    }

    bool saveState (std::string& base64Out) override
    {
        juce::MemoryBlock mb;
        processor.getStateInformation (mb);
        base64Out = juce::Base64::toBase64 (mb.getData(), mb.getSize()).toStdString();
        return true;
    }

private:
    struct ParamRef
    {
        int index = -1;
        juce::AudioProcessorParameter* rp = nullptr;
        juce::RangedAudioParameter*    ranged = nullptr;  // non-null iff the param exposes a NormalisableRange
        bool                           useReal = false;   // ranged AND continuous: value<->norm uses the plugin's curve
        std::string                    id;                // the stable string id (for change events)
    };

    // Map a normalized value to catalog space (real units for a continuous native param, a step index for a
    // discrete one, else the normalized value itself).
    static double valueForCatalog (const ParamRef& ref, float norm)
    {
        if (ref.useReal)
            return (double) ref.ranged->convertFrom0to1 (norm);
        const int steps = ref.rp->getNumSteps();
        if (steps > 1 && ref.rp->isDiscrete())
            return std::round (norm * (double) (steps - 1));
        return (double) norm;
    }

    static void fill (const ParamRef& ref, ParamValue& out, bool withText)
    {
        const float norm = ref.rp->getValue();   // atomic; safe from any thread (normalized 0..1)
        out.normalized = norm;
        out.value      = valueForCatalog (ref, norm);
        if (withText)
            out.text = ref.rp->getText (norm, 256).toStdString();
    }

    // -------- juce::AudioProcessorParameter::Listener --------------------------------------------
    void parameterValueChanged (int index, float) override
    {
        if (auto* s = sink.load())
            s->onParamChanged (index);   // the control plane decides thread handling; we just forward the index
    }
    void parameterGestureChanged (int, bool) override {}

    juce::AudioProcessor&    processor;
    juce::MidiKeyboardState& keyboardState;

    std::unordered_map<std::string, ParamRef> byId;
    std::vector<const ParamRef*>              byIndex;     // parameter index -> ParamRef (for change events)
    std::atomic<ParamChangeSink*>             sink { nullptr };

    JUCE_DECLARE_NON_COPYABLE_WITH_LEAK_DETECTOR (JucePluginBridge)
};

} // namespace sidechain
