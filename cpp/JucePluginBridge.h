#pragma once
#include <juce_audio_processors/juce_audio_processors.h>
#include <juce_audio_basics/juce_audio_basics.h>
#include <juce_dsp/juce_dsp.h>
#include <atomic>
#include <cmath>
#include <cstddef>
#include <memory>
#include <string>
#include <unordered_map>
#include <algorithm>
#include <utility>
#include <vector>
#include "PluginBridge.h"
#include "RenderAnalysis.h"
#include "Sectioning.h"

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

        // The leasable governed sections: the plugin's param groups, or (for a flat plugin) sections derived from
        // shared label prefixes. computeSections (Sectioning.h) is the single source of truth the catalog also uses.
        groups = computeSections (processor).leasable;
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

    std::vector<std::string> sectionGroups() const override { return groups; }

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

    // ---- offline render + measure (message thread only) ----
    // Reset the processor's DSP state (envelopes, LFO phase, filter memory; NOT params, so the current edited patch
    // is what we measure), drive it with a MIDI note (note-driven) or a synthesized input signal (pure effect),
    // loop processBlock over the duration accumulating the output, then analyze. Auto-detects the stimulus from the
    // processor: accepts MIDI => note-driven (input left silent), else a pure effect fed the input signal.
    Measurement renderAndMeasure (const RenderSpec& spec) override
    {
        Measurement out;

        const double sampleRate = processor.getSampleRate() > 0.0 ? processor.getSampleRate() : 48000.0;
        int blockSize = processor.getBlockSize() > 0 ? processor.getBlockSize() : 512;
        if (blockSize <= 0)
            blockSize = 512;

        const int numInputs  = processor.getTotalNumInputChannels();
        const int numOutputs = processor.getTotalNumOutputChannels();
        const int numChannels = juce::jmax (1, numInputs, numOutputs);   // buffer width the processor can use
        if (numOutputs <= 0)
        {
            out.ok = false;
            return out;   // nothing to measure (no audio output)
        }

        // Note-driven if it accepts MIDI; otherwise a pure audio effect. We key on acceptsMidi ALONE, not
        // (acceptsMidi && numInputs==0): many synths expose an audio-input bus too (Surge XT reports 2 inputs for
        // its audio-in oscillator / vocoder), so gating on zero inputs misclassifies them as effects and renders
        // silence. A note-driven plugin gets the note with its input left silent (measuring what it SYNTHESISES,
        // not leaked input); a pure effect gets the input signal and ignores note/gate. The rare MIDI-controlled
        // effect lands in the note-driven branch, which is the correct stimulus for it.
        const bool isInstrument = processor.acceptsMidi();

        const int durationMs = juce::jmax (1, spec.durationMs);
        const int totalSamples = (int) std::llround ((double) durationMs * sampleRate / 1000.0);
        if (totalSamples <= 0)
        {
            out.ok = false;
            return out;
        }

        // Reset DSP state (filter memory, envelopes, LFO phase, reverb tails) so each render is deterministic and
        // free of the prior render's tail, then drive a fresh note. Use reset(), NOT releaseResources() +
        // prepareToPlay(): the release/prepare cycle makes the hosted plugin rebuild its DSP from its patch/default
        // state and drops the parameter values pushed in via set_param, so every render came out identical to the
        // default patch regardless of edits (a filter cutoff change had zero audible effect). reset() clears state
        // while preserving the applied parameters. The host already prepared the processor at load (Host::load) at
        // this same rate/block, so no re-prepare is needed.
        processor.reset();

        // Note-on at sample 0, note-off at gateMs (clamped into the render). Only meaningful for an instrument.
        const int gateSample = juce::jlimit (0, totalSamples,
                                             (int) std::llround ((double) juce::jmax (0, spec.gateMs) * sampleRate / 1000.0));
        const int channel = juce::jlimit (1, 16, spec.channel);
        const int note    = juce::jlimit (0, 127, spec.note);
        const float vel   = juce::jlimit (0.0f, 1.0f, spec.velocity);

        // Build the MIDI schedule: a single held note, or a PHRASE (chord / arp / sequence) when spec.notes is set.
        // Each entry is (absoluteSample, message), sorted by time; the block loop emits the events in each block's
        // window. noteOffSample tracks the LAST note-off, which bounds the temporal analysis sustain window below.
        std::vector<std::pair<int, juce::MidiMessage>> sched;
        int noteOffSample = gateSample;
        if (isInstrument)
        {
            if (! spec.notes.empty())
            {
                noteOffSample = 0;
                for (std::size_t i = 0; i < spec.notes.size(); ++i)
                {
                    const auto& ne = spec.notes[i];
                    const int   ch = spec.mpe ? (int) (i % 15) + 2 : 1;   // MPE member channels 2..16, else channel 1
                    const int   nn = juce::jlimit (0, 127, ne.note);
                    const float nv = juce::jlimit (0.0f, 1.0f, ne.velocity);
                    const int startMs = juce::jmax (0, ne.startMs);
                    const int onS  = juce::jlimit (0, juce::jmax (0, totalSamples - 1),
                                                   (int) std::llround ((double) startMs * sampleRate / 1000.0));
                    const int offS = juce::jlimit (onS + 1, totalSamples,
                                                   (int) std::llround ((double) (startMs + juce::jmax (1, ne.gateMs)) * sampleRate / 1000.0));
                    if (ne.bend < 0.0f || ne.bend > 0.0f)   // non-zero (relational form: -Wfloat-equal safe)
                    {
                        const double bendRange = spec.mpe ? 48.0 : 2.0;   // assumed pitch-wheel range in semitones
                        const int wheel = juce::jlimit (0, 16383, 8192 + (int) std::llround ((double) ne.bend / bendRange * 8191.0));
                        sched.emplace_back (onS, juce::MidiMessage::pitchWheel (ch, wheel));
                    }
                    if (ne.pressure > 0.0f)
                        sched.emplace_back (onS, juce::MidiMessage::channelPressureChange (ch, juce::jlimit (0, 127, (int) std::llround ((double) ne.pressure * 127.0))));
                    sched.emplace_back (onS, juce::MidiMessage::noteOn (ch, nn, nv));
                    sched.emplace_back (offS, juce::MidiMessage::noteOff (ch, nn));
                    noteOffSample = juce::jmax (noteOffSample, offS);
                }
                std::sort (sched.begin(), sched.end(),
                           [] (const std::pair<int, juce::MidiMessage>& a, const std::pair<int, juce::MidiMessage>& b) { return a.first < b.first; });
            }
            else
            {
                sched.emplace_back (0, juce::MidiMessage::noteOn (channel, note, vel));
                sched.emplace_back (gateSample, juce::MidiMessage::noteOff (channel, note));
            }
        }

        // Accumulate the whole render summed to mono for the headline numbers (matches the analysis contract).
        std::vector<float> mono;
        mono.reserve ((std::size_t) totalSamples);

        juce::AudioBuffer<float> buffer (numChannels, blockSize);
        juce::uint64 inputPhaseSamples = 0;   // running sample index, for a continuous input sine across blocks
        juce::Random rng (0xC0FFEE);          // seeded => deterministic noise

        int pos = 0;
        while (pos < totalSamples)
        {
            const int thisBlock = juce::jmin (blockSize, totalSamples - pos);
            buffer.clear();

            // ---- build the input for an effect (an instrument leaves the input silent) ----
            if (! isInstrument && numInputs > 0)
                fillInput (buffer, numInputs, thisBlock, spec, sampleRate, inputPhaseSamples, rng);
            inputPhaseSamples += (juce::uint64) thisBlock;

            // ---- MIDI for an instrument: emit every scheduled event that falls in this block ----
            juce::MidiBuffer midi;
            for (const auto& ev : sched)
                if (ev.first >= pos && ev.first < pos + thisBlock)
                    midi.addEvent (ev.second, ev.first - pos);

            // Present exactly thisBlock samples to the processor, then accumulate.
            juce::AudioBuffer<float> view (buffer.getArrayOfWritePointers(), numChannels, thisBlock);
            processor.processBlock (view, midi);

            appendMono (mono, view, numOutputs, thisBlock);
            pos += thisBlock;
        }

        // ---- fill the measurement: pure core (RenderAnalysis.h) + the FFT path ----
        const BasicMeasurement basic = analyzeMono (mono.data(), mono.size());
        out.ok          = true;
        out.durationSec = (double) totalSamples / sampleRate;
        out.sampleRate  = sampleRate;
        out.channels    = numOutputs;
        out.peakDb      = basic.peakDb;
        out.rmsDb       = basic.rmsDb;
        out.crest       = basic.crest;
        out.silent      = basic.silent;
        out.clipped     = basic.clipped;

        analyzeSpectrum (mono, sampleRate, out);

        // ---- Tier 2.5: temporal (modulation-aware) measurement (only when requested) ----
        // Slides non-overlapping frames over the mono buffer. For each frame compute the RMS (dBFS) and
        // the spectral centroid (Hz) using the same per-frame FFT approach as analyzeSpectrum but applied to
        // a single frame. This builds two envelope time series (centroidHz[], rmsDb[]) that then feed the
        // pure analyzeEnvelope estimator (RenderAnalysis.h) to detect LFO rate/depth/regularity.
        if (spec.temporal)
        {
            // Analyze the SUSTAIN region only, not the whole render. The note's own amplitude envelope (the attack
            // transient, and for an instrument the release decay to silence after note-off) is a large one-shot
            // excursion that otherwise swamps the LFO and reports as huge depth with no periodicity. Skip a short
            // attack guard at the start, and for an instrument stop at note-off (gateSample) so the release tail is
            // excluded; for an effect the excitation is sustained the whole render, so analyze to the end.
            const int modFrameMs  = juce::jlimit (5, 100, spec.frameMs > 0 ? spec.frameMs : 25);
            const int modFrameLen = juce::jmax (1, (int) std::llround ((double) modFrameMs * sampleRate / 1000.0));
            const int guard  = (int) std::llround (0.20 * sampleRate);            // 200 ms attack guard
            int aStart = juce::jmin (guard, totalSamples / 4);
            int aEnd   = isInstrument ? juce::jmin (noteOffSample, totalSamples) : totalSamples;
            if (aEnd - aStart < 8 * modFrameLen)                                  // too little sustain to trust a window
            {
                aStart = 0;                                                       // fall back to the whole buffer
                aEnd   = totalSamples;
            }
            analyzeModulation (mono, sampleRate, spec, aStart, aEnd, out);
        }

        return out;
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

    // -------- render helpers (message thread) ----------------------------------------------------
    // Synthesize the excitation signal into every input channel for the current block. `phaseSamples` is the
    // running sample index across the whole render, so a sine is phase-continuous between blocks.
    static void fillInput (juce::AudioBuffer<float>& buffer, int numInputs, int numSamples,
                           const RenderSpec& spec, double sampleRate, juce::uint64 phaseSamples, juce::Random& rng)
    {
        const float level = juce::jlimit (0.0f, 1.0f, spec.inputLevel);
        for (int i = 0; i < numSamples; ++i)
        {
            float s = 0.0f;
            switch (spec.inputKind)
            {
                case InputKind::Silence:
                    s = 0.0f;
                    break;
                case InputKind::Sine:
                {
                    const double t = (double) (phaseSamples + (juce::uint64) i) / sampleRate;
                    s = level * (float) std::sin (juce::MathConstants<double>::twoPi * spec.inputFreq * t);
                    break;
                }
                case InputKind::Noise:
                    s = level * (rng.nextFloat() * 2.0f - 1.0f);   // white noise in [-level, level]
                    break;
                case InputKind::Impulse:
                    s = (phaseSamples == 0 && i == 0) ? level : 0.0f;   // a single unit sample at t=0
                    break;
            }
            for (int ch = 0; ch < numInputs; ++ch)
                buffer.setSample (ch, i, s);
        }
    }

    // Append this block's output, summed across channels to mono (matching the measurement contract).
    static void appendMono (std::vector<float>& mono, const juce::AudioBuffer<float>& view, int numOutputs, int numSamples)
    {
        const int chans = juce::jmin (numOutputs, view.getNumChannels());
        for (int i = 0; i < numSamples; ++i)
        {
            float sum = 0.0f;
            for (int ch = 0; ch < chans; ++ch)
                sum += view.getSample (ch, i);
            mono.push_back (chans > 0 ? sum / (float) chans : 0.0f);
        }
    }

    // The FFT-based measures (spectral centroid in Hz; 3-band energy low <200 / mid 200-2000 / high >2000, dBFS).
    // Averages the magnitude spectrum over successive Hann-windowed frames (juce::dsp::FFT), so the numbers are
    // stable over the whole render rather than a single frame. Leaves the defaults (silent floor) on empty input.
    static void analyzeSpectrum (const std::vector<float>& mono, double sampleRate, Measurement& out)
    {
        if (mono.empty() || sampleRate <= 0.0)
            return;

        constexpr int fftOrder = 11;                 // 2048-point FFT: about 23 Hz bins at 48 kHz
        constexpr int fftSize  = 1 << fftOrder;
        const int numBins = fftSize / 2;             // real-signal spectrum is symmetric; keep the lower half
        if ((int) mono.size() < fftSize)
            return;                                   // too short for one frame; leave FFT measures at the floor

        juce::dsp::FFT fft (fftOrder);
        juce::dsp::WindowingFunction<float> window ((std::size_t) fftSize, juce::dsp::WindowingFunction<float>::hann);

        std::vector<float> avgMag ((std::size_t) numBins, 0.0f);
        const int hop = fftSize / 2;                 // 50% overlap
        int frames = 0;
        std::vector<float> fftData ((std::size_t) (fftSize * 2), 0.0f);   // in-place complex buffer for JUCE FFT

        for (std::size_t start = 0; start + (std::size_t) fftSize <= mono.size(); start += (std::size_t) hop)
        {
            std::fill (fftData.begin(), fftData.end(), 0.0f);
            for (int i = 0; i < fftSize; ++i)
                fftData[(std::size_t) i] = mono[start + (std::size_t) i];
            window.multiplyWithWindowingTable (fftData.data(), (std::size_t) fftSize);
            fft.performFrequencyOnlyForwardTransform (fftData.data());   // magnitudes land in [0, fftSize)
            for (int b = 0; b < numBins; ++b)
                avgMag[(std::size_t) b] += fftData[(std::size_t) b];
            ++frames;
        }
        if (frames == 0)
            return;

        const double binHz = sampleRate / (double) fftSize;
        double weightedFreq = 0.0, magSum = 0.0;
        double lowE = 0.0, midE = 0.0, highE = 0.0;   // summed power per band
        for (int b = 1; b < numBins; ++b)             // skip DC (bin 0) for the centroid / bands
        {
            const double mag = (double) avgMag[(std::size_t) b] / (double) frames;
            const double freq = (double) b * binHz;
            weightedFreq += freq * mag;
            magSum       += mag;

            const double power = mag * mag;
            if (freq < 200.0)        lowE  += power;
            else if (freq <= 2000.0) midE  += power;
            else                     highE += power;
        }

        out.centroidHz = (magSum > 0.0) ? (weightedFreq / magSum) : 0.0;
        // Band energy as an RMS-like dBFS level (sqrt of summed power, normalized by the FFT size so full-scale
        // maps near 0 dBFS). These are relative brightness/tilt indicators, not calibrated absolute levels.
        const double norm = (double) fftSize;
        out.lowDb  = linearToDb (std::sqrt (lowE)  / norm);
        out.midDb  = linearToDb (std::sqrt (midE)  / norm);
        out.highDb = linearToDb (std::sqrt (highE) / norm);
    }

    // -------- Tier 2.5: temporal modulation analysis (message thread) ---------------------------
    // Slides non-overlapping frames over the mono buffer. Per frame: compute rms (dBFS) and spectral centroid
    // (Hz via a single windowed FFT). Build two envelope series; run analyzeEnvelope on each to estimate LFO
    // rate/depth. Pick the dominant signal by a normalized strength score; fill measurement.modulation.
    static void analyzeModulation (const std::vector<float>& mono, double sampleRate,
                                   const RenderSpec& spec, int windowStart, int windowEnd, Measurement& out)
    {
        // Clamp frameMs to the [5, 100] range (per the wire contract); then compute frame length in samples.
        const int frameMs  = juce::jlimit (5, 100, spec.frameMs > 0 ? spec.frameMs : 25);
        const int frameLen = juce::jmax (1, (int) std::llround ((double) frameMs * sampleRate / 1000.0));

        // The envelope sample rate: one measurement per frame.
        const double fsEnv = sampleRate / (double) frameLen;

        // Frame only the analysis WINDOW (the sustain region the caller selected), not the whole buffer.
        const int total       = (int) mono.size();
        const int winStart    = juce::jlimit (0, total, windowStart);
        const int winEnd       = juce::jlimit (winStart, total, windowEnd);
        const int windowLen    = winEnd - winStart;
        const int numFrames    = windowLen / frameLen;   // integer; the last partial frame is dropped

        // Always fill the block as present=true (with potentially low confidence) so the caller can see the
        // frame parameters; we just cannot reliably estimate a rate if we have fewer than 4 frames.
        out.modulation.present  = true;
        out.modulation.frameMs  = frameMs;

        if (numFrames < 1)
            return;   // present=true but all signals stay at defaults (zero confidence)

        // Per-frame FFT setup (reuse the same FFT order as analyzeSpectrum when the frame is long enough;
        // otherwise use the largest order that fits). A single windowed FFT per frame is sufficient.
        int fftOrder = 11;   // 2048-point is the default
        while (fftOrder > 1 && (1 << fftOrder) > frameLen)
            --fftOrder;
        const int fftSize = 1 << fftOrder;
        const int numBins = fftSize / 2;
        const double binHz = sampleRate / (double) fftSize;

        // Only allocate FFT objects when a frame is long enough for at least one bin past DC.
        const bool canFFT = (frameLen >= fftSize && fftSize >= 4);
        juce::dsp::FFT                          fft (fftOrder);
        juce::dsp::WindowingFunction<float>     window ((std::size_t) fftSize, juce::dsp::WindowingFunction<float>::hann);
        std::vector<float>                      fftData ((std::size_t) (fftSize * 2), 0.0f);

        // Envelope time series: one value per frame.
        std::vector<float> centroidEnv ((std::size_t) numFrames, 0.0f);
        std::vector<float> rmsDbEnv    ((std::size_t) numFrames, 0.0f);
        std::vector<float> f0Env       ((std::size_t) numFrames, 0.0f);   // fundamental in semitones (for vibrato)
        double lastVoicedSemi = 0.0;   // held through unvoiced frames so the pitch envelope stays continuous
        int    firstVoiced    = -1;    // index of the first voiced frame (for backfilling leading unvoiced frames)

        for (int f = 0; f < numFrames; ++f)
        {
            const int start = winStart + f * frameLen;

            // RMS of this frame (dBFS).
            double sumSq = 0.0;
            for (int i = 0; i < frameLen; ++i)
            {
                const double s = (double) mono[(std::size_t) (start + i)];
                sumSq += s * s;
            }
            const double rmsLinear = std::sqrt (sumSq / (double) frameLen);
            rmsDbEnv[(std::size_t) f] = (float) linearToDb (rmsLinear);

            // Fundamental frequency of this frame -> semitones (vibrato tracking). Unvoiced/silent frames hold the
            // last voiced value so the envelope has no artificial jumps.
            const double f0 = estimateF0 (mono.data() + start, (std::size_t) frameLen, sampleRate);
            if (f0 > 0.0)
            {
                lastVoicedSemi = hzToSemitones (f0);
                if (firstVoiced < 0)
                    firstVoiced = f;
            }
            f0Env[(std::size_t) f] = (float) lastVoicedSemi;

            // Spectral centroid of this frame via a single windowed FFT (only when the frame is long enough).
            if (canFFT)
            {
                std::fill (fftData.begin(), fftData.end(), 0.0f);
                for (int i = 0; i < fftSize; ++i)
                    fftData[(std::size_t) i] = mono[(std::size_t) (start + i)];
                window.multiplyWithWindowingTable (fftData.data(), (std::size_t) fftSize);
                fft.performFrequencyOnlyForwardTransform (fftData.data());
                double wFreq = 0.0, mSum = 0.0;
                for (int b = 1; b < numBins; ++b)
                {
                    const double mag  = (double) fftData[(std::size_t) b];
                    const double freq = (double) b * binHz;
                    wFreq += freq * mag;
                    mSum  += mag;
                }
                centroidEnv[(std::size_t) f] = (mSum > 0.0) ? (float) (wFreq / mSum) : 0.0f;
            }
            // If canFFT is false (very short frame), centroid stays 0 for this frame.
        }

        // Backfill leading unvoiced frames with the first voiced pitch, so a slow note onset does not read as a
        // large one-shot pitch excursion. If nothing was ever voiced, the envelope stays flat (no pitch modulation).
        if (firstVoiced > 0)
            for (int f = 0; f < firstVoiced; ++f)
                f0Env[(std::size_t) f] = f0Env[(std::size_t) firstVoiced];

        // Analyze each envelope for periodicity using the pure JUCE-free estimator.
        const EnvStats centStat  = analyzeEnvelope (centroidEnv.data(), (std::size_t) numFrames, fsEnv);
        const EnvStats rmsStat   = analyzeEnvelope (rmsDbEnv.data(),    (std::size_t) numFrames, fsEnv);
        const EnvStats pitchStat = analyzeEnvelope (f0Env.data(),       (std::size_t) numFrames, fsEnv);

        // Fill the wire-visible ModSignal structs.
        out.modulation.centroid.rateHz     = centStat.rateHz;
        out.modulation.centroid.depth      = centStat.depth;
        out.modulation.centroid.confidence = centStat.confidence;
        out.modulation.centroid.regular    = centStat.regular;

        out.modulation.rms.rateHz     = rmsStat.rateHz;
        out.modulation.rms.depth      = rmsStat.depth;
        out.modulation.rms.confidence = rmsStat.confidence;
        out.modulation.rms.regular    = rmsStat.regular;

        out.modulation.pitch.rateHz     = pitchStat.rateHz;
        out.modulation.pitch.depth      = pitchStat.depth;   // semitones (peak-to-peak vibrato)
        out.modulation.pitch.confidence = pitchStat.confidence;
        out.modulation.pitch.regular    = pitchStat.regular;

        // dominant: compare normalized strength scores. Normalize centroid depth by 2000 Hz, rms by 12 dB, and pitch
        // by 1 semitone so all three are on roughly the same [0, 1] scale. A signal only scores above zero when
        // regular is true; irregular movement still gets a depth measurement but does not claim to be dominant.
        const double centScore  = centStat.regular  ? centStat.confidence  * (centStat.depth  / 2000.0) : 0.0;
        const double rmsScore   = rmsStat.regular   ? rmsStat.confidence   * (rmsStat.depth   / 12.0)   : 0.0;
        const double pitchScore = pitchStat.regular ? pitchStat.confidence * (pitchStat.depth / 1.0)    : 0.0;
        const double kDomThreshold = 1.0e-4;   // below this, none is meaningfully dominant
        if (centScore >= rmsScore && centScore >= pitchScore && centScore > kDomThreshold)
            out.modulation.dominant = "centroid";
        else if (rmsScore >= centScore && rmsScore >= pitchScore && rmsScore > kDomThreshold)
            out.modulation.dominant = "rms";
        else if (pitchScore > kDomThreshold)
            out.modulation.dominant = "pitch";
        else
            out.modulation.dominant = "none";
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
    std::vector<std::string>                  groups;      // distinct param groups (leasable sections), first-seen order
    std::atomic<ParamChangeSink*>             sink { nullptr };

    JUCE_DECLARE_NON_COPYABLE_WITH_LEAK_DETECTOR (JucePluginBridge)
};

} // namespace sidechain
