#pragma once
#include <juce_audio_processors/juce_audio_processors.h>
#include <juce_audio_basics/juce_audio_basics.h>
#include <atomic>
#include <cmath>
#include <functional>
#include <string>
#include <unordered_map>

// ================================================================================================
// sidechain::ControlListener - an in-process LIVE-CONTROL listener for any juce::AudioProcessor.
//
// Point it at a processor (your own plugin, or a child plugin instance loaded via
// juce::AudioPluginFormatManager - see Host.h) and an external agent can drive it in real time over a
// localhost socket: move any parameter, play notes, ask for a parameter's current value, and get/set the
// opaque full-state blob. The Go MCP server (this repo) is a thin forwarder that speaks the wire protocol
// below 1:1.
//
// THREAD MODEL (the load-bearing safety rule):
//   • The SOCKET runs on its OWN background thread (this : juce::Thread). It NEVER touches parameter/DSP state
//     directly (that would race processBlock). It only: (a) parses a line of JSON; (b) for MUTATIONS,
//     resolves the target + pushes a Command into a lock-free SPSC queue and fires an AsyncUpdate, then waits
//     (bounded) for the message thread to apply it before acking; (c) for READS, snapshots the parameter's
//     atomic value and replies directly (reads are safe from any thread).
//   • The MESSAGE THREAD drains the queue (this : juce::AsyncUpdater → handleAsyncUpdate) and applies each
//     Command via setValueNotifyingHost / keyboardState - exactly the on-screen-keyboard / GUI-edit path, so
//     the editor and the host see the change and it serializes normally.
//
// TRANSPORT - localhost TCP, line-delimited JSON, request/response. Bound to 127.0.0.1 ONLY (never a routable
// interface). One client at a time. Chosen over UDP/OSC because MCP is request/response (the agent needs acks
// + state read-back). Commands mirror the MCP tools 1:1:
//   ping · get_param{param} · set_param{param,(value|normalized|choice)} · note_on{chan,note,vel} ·
//   note_off{chan,note} · all_notes_off · reset_init · load_state{xml} · get_full_state · get_state.
// ================================================================================================

namespace sidechain
{

class ControlListener : private juce::Thread,
                        private juce::AsyncUpdater
{
public:
    // Session status. Ordered idle < listening < connected.
    enum class Status { Idle, Listening, Connected };

    static constexpr int kDefaultPort = 51703;   // 127.0.0.1:51703 (the Go server's connect_live default)

    // `proc` and `kbd` are JUCE base types, so this header has NO dependency on any concrete processor type
    // (avoids a circular include and keeps it a drop-in for a hosted child plugin). The listener walks
    // proc.getParameters() to resolve param ids.
    ControlListener (juce::AudioProcessor& proc, juce::MidiKeyboardState& kbd, int port = kDefaultPort)
        : juce::Thread ("sidechain-control"), processor (proc), keyboardState (kbd), listenPort (port)
    {
        // Build the id → param map ONCE (at construction) on the BASE AudioProcessorParameter API. A HOSTED
        // VST3/AU exposes parameters as juce::HostedAudioProcessorParameter (NOT RangedAudioParameter), so a
        // RangedAudioParameter cast would drop every hosted param; the base API works for both our own params
        // and hosted ones. The stable string id comes from a HostedAudioProcessorParameter when available,
        // else the parameter index (matches Host::enumerateCatalog).
        const auto& params = processor.getParameters();
        for (int i = 0; i < params.size(); ++i)
        {
            auto* p = params[i];
            std::string id = std::to_string (i);
            if (auto* hp = dynamic_cast<juce::HostedAudioProcessorParameter*> (p))
                if (hp->getParameterID().isNotEmpty())
                    id = hp->getParameterID().toStdString();
            // A NATIVE param (our own plugin) is a RangedAudioParameter: its NormalisableRange owns the
            // real<->normalized curve (including skew for freq/gain/time). A HOSTED VST3/AU param is not, so it
            // has only the normalized 0..1 scalar. `useReal` (ranged AND continuous) is the flag that routes
            // value<->norm through the plugin's own curve instead of a linear approximation.
            auto* ranged = dynamic_cast<juce::RangedAudioParameter*> (p);
            byId[id] = ParamRef { i, p, ranged, ranged != nullptr && ! p->isDiscrete() };
        }

        status.store (Status::Listening);
        startThread (juce::Thread::Priority::normal);
    }

    ~ControlListener() override
    {
        signalThreadShouldExit();
        listener.close();     // unblocks waitForNextConnection() / a blocked read on the accept thread
        client.close();
        stopThread (2000);
        cancelPendingUpdate();
        status.store (Status::Idle);
    }

    Status getStatus() const noexcept { return status.load(); }
    int     getPort()   const noexcept { return listenPort; }

    // Optional message-thread action for the "reset_init" command (recall an init/default patch). A host may
    // wire this to its own reset logic; it runs on the message-thread drain, so it is safe to touch parameter
    // state and editor-owned data. Null ⇒ the command is a no-op ack. Kept as a callback so this header stays
    // free of any concrete processor dependency.
    std::function<void()> onResetInit;

private:
    struct ParamRef
    {
        int index = -1;
        juce::AudioProcessorParameter* rp = nullptr;
        juce::RangedAudioParameter*    ranged = nullptr;  // non-null iff the param exposes a NormalisableRange
        bool                           useReal = false;   // ranged AND continuous: value<->norm uses the plugin's (skew-aware) curve
    };

    enum class Kind : uint8_t { SetParam, NoteOn, NoteOff, AllNotesOff, ResetInit, LoadState, GetFullState };
    struct Command
    {
        Kind  kind;
        int   a = 0;        // SetParam: param index · NoteOn/Off: channel
        int   b = 0;        // NoteOn/Off: note number
        float f = 0.0f;     // SetParam: normalized value · NoteOn: velocity (0..1)
    };

    // -------- socket accept/read loop (background thread) ----------------------------------------
    void run() override
    {
        // Bind 127.0.0.1 ONLY. createListener with an explicit local host never exposes a routable iface.
        if (! listener.createListener (listenPort, "127.0.0.1"))
        {
            status.store (Status::Idle);
            return;
        }

        while (! threadShouldExit())
        {
            std::unique_ptr<juce::StreamingSocket> conn (listener.waitForNextConnection());
            if (conn == nullptr)
                break;   // listener closed (shutdown) or fatal error

            status.store (Status::Connected);
            serveClient (*conn);
            status.store (threadShouldExit() ? Status::Idle : Status::Listening);
        }
    }

    void serveClient (juce::StreamingSocket& conn)
    {
        std::string inbox;
        char buf[2048];

        while (! threadShouldExit() && conn.isConnected())
        {
            const int got = conn.read (buf, (int) sizeof (buf), false);
            if (got < 0) break;            // error / disconnect
            if (got == 0)                  // no data ready
            {
                if (conn.waitUntilReady (true, 200) < 0) break;
                continue;
            }

            inbox.append (buf, (size_t) got);
            for (auto nl = inbox.find ('\n'); nl != std::string::npos; nl = inbox.find ('\n'))
            {
                const std::string line = inbox.substr (0, nl);
                inbox.erase (0, nl + 1);
                if (line.find_first_not_of (" \t\r") == std::string::npos)
                    continue;

                const juce::String reply = handleLine (line);
                const juce::String out   = reply + "\n";
                if (conn.write (out.toRawUTF8(), (int) out.getNumBytesAsUTF8()) < 0)
                    return;
            }
        }
    }

    // Parse one JSON request line and produce one JSON reply line (socket thread).
    juce::String handleLine (const std::string& line)
    {
        juce::var req = juce::JSON::parse (juce::String (juce::CharPointer_UTF8 (line.c_str())));
        if (! req.isObject())
            return errorReply ("bad_json", {});

        const juce::String cmd = req.getProperty ("cmd", juce::var()).toString();
        const juce::var    id  = req.getProperty ("id", juce::var());   // optional request correlation id

        if (cmd == "ping")
            return okReply (id, [] (juce::DynamicObject& o) { o.setProperty ("pong", true); });

        if (cmd == "get_param")
            return handleGetParam (req, id);

        if (cmd == "set_param")
            return handleSetParam (req, id);

        if (cmd == "note_on" || cmd == "note_off")
            return handleNote (req, id, cmd == "note_on");

        if (cmd == "all_notes_off")
        {
            enqueue ({ Kind::AllNotesOff }, /*ack*/ true);
            return okReply (id, [] (juce::DynamicObject&) {});
        }

        if (cmd == "reset_init")   // recall an init/default patch (host-supplied), message-thread
        {
            appliedEvent.reset();
            enqueue ({ Kind::ResetInit }, /*ack*/ true);
            appliedEvent.wait (500);   // may touch every param; give the drain a moment before acking
            return okReply (id, [] (juce::DynamicObject& o) { o.setProperty ("reset", true); });
        }

        if (cmd == "load_state")   // push a WHOLE patch (the plugin's opaque state XML) into the live instance
        {
            pendingLoadXml = req.getProperty ("xml", juce::var()).toString().toStdString();
            if (pendingLoadXml.empty())
                return errorReply ("no_xml", id);
            lastLoadOk = false;
            appliedEvent.reset();
            enqueue ({ Kind::LoadState }, /*ack*/ true);
            appliedEvent.wait (2000);   // a full-state restore can be heavier than a param set
            return lastLoadOk ? okReply (id, [] (juce::DynamicObject& o) { o.setProperty ("loaded", true); })
                              : errorReply ("load_failed", id);
        }

        if (cmd == "get_full_state")   // pull the WHOLE live patch as the plugin's opaque state XML
        {
            fetchedStateXml.clear();
            appliedEvent.reset();
            enqueue ({ Kind::GetFullState }, /*ack*/ true);
            appliedEvent.wait (2000);
            if (fetchedStateXml.empty())
                return errorReply ("state_unavailable", id);
            return okReply (id, [this] (juce::DynamicObject& o) { o.setProperty ("xml", juce::String (fetchedStateXml)); });
        }

        if (cmd == "get_state")
            return handleGetState (id);

        return errorReply ("unknown_cmd", id);
    }

    juce::String handleGetParam (const juce::var& req, const juce::var& id)
    {
        const std::string pid = req.getProperty ("param", juce::var()).toString().toStdString();
        auto it = byId.find (pid);
        if (it == byId.end())
            return errorReply ("unknown_param", id);

        auto* rp = it->second.rp;
        const float norm = rp->getValue();                 // atomic; safe from any thread (normalized 0..1)
        // A hosted parameter has no separate real-unit scalar; its authoritative human form is the text the
        // plugin renders. `value` mirrors the discrete index (steps) or the normalized float, matching the
        // catalog ranges Host::enumerateCatalog reports; `text` is the plugin's own formatting.
        const juce::String text = rp->getText (norm, 256);
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("param",      juce::String (pid));
            o.setProperty ("value",      valueForCatalog (it->second, norm));
            o.setProperty ("normalized", norm);
            o.setProperty ("text",       text);
        });
    }

    juce::String handleSetParam (const juce::var& req, const juce::var& id)
    {
        const std::string pid = req.getProperty ("param", juce::var()).toString().toStdString();
        auto it = byId.find (pid);
        if (it == byId.end())
            return errorReply ("unknown_param", id);

        auto* rp = it->second.rp;
        const int steps = rp->getNumSteps();
        float norm;
        if (req.hasProperty ("normalized"))
            norm = juce::jlimit (0.0f, 1.0f, (float) req.getProperty ("normalized", 0.0f));
        else if (req.hasProperty ("choice"))
        {
            // choice index → normalized. A discrete param maps index i to i/(steps-1).
            const int idx = (int) req.getProperty ("choice", 0);
            norm = (steps > 1) ? juce::jlimit (0.0f, 1.0f, (float) idx / (float) (steps - 1)) : 0.0f;
        }
        else if (req.hasProperty ("value"))
        {
            const float v = (float) req.getProperty ("value", 0.0f);
            if (it->second.useReal)
                // A continuous NATIVE param: `value` is REAL units; convert through the plugin's own
                // (possibly skewed) NormalisableRange, NOT a linear map, so 500 Hz on a log range lands right.
                norm = juce::jlimit (0.0f, 1.0f, it->second.ranged->convertTo0to1 (v));
            else
                // A HOSTED param: `value` for a discrete param IS the index; for a continuous one it is already
                // the normalized 0..1 (the base API exposes no separate real scalar).
                norm = (steps > 1) ? juce::jlimit (0.0f, 1.0f, v / (float) (steps - 1))
                                   : juce::jlimit (0.0f, 1.0f, v);
        }
        else
            return errorReply ("no_value", id);

        // Queue for the message thread; wait (bounded) so the ack means "applied", giving the agent clean
        // request/response semantics (set → the value is live before the reply lands).
        appliedEvent.reset();
        enqueue ({ Kind::SetParam, it->second.index, 0, norm }, /*ack*/ true);
        appliedEvent.wait (250);

        const float applied = rp->getValue();
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("param",      juce::String (pid));
            o.setProperty ("normalized", applied);
            o.setProperty ("value",      valueForCatalog (it->second, applied));
            o.setProperty ("text",       rp->getText (applied, 256));
        });
    }

    juce::String handleNote (const juce::var& req, const juce::var& id, bool on)
    {
        const int   chan = juce::jlimit (1, 16,  (int)   req.getProperty ("chan", 1));
        const int   note = juce::jlimit (0, 127, (int)   req.getProperty ("note", 60));
        const float vel  = juce::jlimit (0.0f, 1.0f, (float) req.getProperty ("vel", 0.8));
        enqueue ({ on ? Kind::NoteOn : Kind::NoteOff, chan, note, vel }, /*ack*/ true);
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("note", note);
            o.setProperty ("on",   on);
        });
    }

    // Session snapshot: every resolved param's current value (id → normalized/value). Reads only (atomic
    // getValue), so it is safe from the socket thread.
    juce::String handleGetState (const juce::var& id)
    {
        auto* params = new juce::DynamicObject();
        for (const auto& kv : byId)
        {
            auto* rp = kv.second.rp;
            const float norm = rp->getValue();
            auto* one = new juce::DynamicObject();
            one->setProperty ("value",      valueForCatalog (kv.second, norm));
            one->setProperty ("normalized", norm);
            params->setProperty (juce::String (kv.first), juce::var (one));
        }
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("count",  (int) byId.size());
            o.setProperty ("params", juce::var (params));
        });
    }

    // -------- lock-free SPSC command queue (socket producer → message-thread consumer) -----------
    void enqueue (const Command& c, bool wantsAck)
    {
        int start1, size1, start2, size2;
        fifo.prepareToWrite (1, start1, size1, start2, size2);
        if (size1 > 0)
        {
            ring[(size_t) start1] = c;
            wantAck[(size_t) start1] = wantsAck;
            fifo.finishedWrite (1);
            triggerAsyncUpdate();
        }
        // Ring full (256 deep): drop. A human/agent issues commands far slower than a 20 ms drain; a drop here
        // only means one command didn't land, never a crash or a race.
    }

    void handleAsyncUpdate() override
    {
        bool ackAny = false;
        int start1, size1, start2, size2;
        fifo.prepareToRead (fifo.getNumReady(), start1, size1, start2, size2);
        auto apply = [&] (int base, int n)
        {
            for (int i = 0; i < n; ++i)
            {
                const Command& c = ring[(size_t) (base + i)];
                applyCommand (c);
                ackAny |= wantAck[(size_t) (base + i)];
            }
        };
        apply (start1, size1);
        apply (start2, size2);
        fifo.finishedRead (size1 + size2);
        if (ackAny)
            appliedEvent.signal();
    }

    void applyCommand (const Command& c)
    {
        switch (c.kind)
        {
            case Kind::SetParam:
                if (auto* p = processor.getParameters()[c.a])
                    p->setValueNotifyingHost (c.f);     // same path as a GUI knob / host automation
                break;
            case Kind::NoteOn:      keyboardState.noteOn  (c.a, c.b, c.f); break;
            case Kind::NoteOff:     keyboardState.noteOff (c.a, c.b, c.f); break;
            case Kind::AllNotesOff:
                for (int ch = 1; ch <= 16; ++ch) keyboardState.allNotesOff (ch);
                break;
            case Kind::ResetInit:
                if (onResetInit) onResetInit();   // host-supplied, on the message thread
                break;
            case Kind::LoadState:
                // Parse the incoming state XML → the binary blob setStateInformation expects, then apply it via
                // the NORMAL host state path. juce::AudioProcessor-only, so this header needs no processor type.
                if (auto xml = juce::parseXML (juce::String (pendingLoadXml)))
                {
                    juce::MemoryBlock mb;
                    juce::AudioProcessor::copyXmlToBinary (*xml, mb);
                    processor.setStateInformation (mb.getData(), (int) mb.getSize());
                    lastLoadOk = true;
                }
                break;
            case Kind::GetFullState:
                // Snapshot the live patch the same way the host saves it, then hand back the XML text.
                {
                    juce::MemoryBlock mb;
                    processor.getStateInformation (mb);
                    if (auto xml = juce::AudioProcessor::getXmlFromBinary (mb.getData(), (int) mb.getSize()))
                        fetchedStateXml = xml->toString().toStdString();
                }
                break;
        }
    }

    // Map a normalized 0..1 value to the "value" the catalog uses. For a continuous NATIVE (ranged) param this
    // is the REAL value via the plugin's own (skew-aware) NormalisableRange; for a discrete param it is the
    // index (0..steps-1); for a hosted continuous param it is the normalized value itself (no real scalar).
    static double valueForCatalog (const ParamRef& ref, float norm)
    {
        if (ref.useReal)
            return (double) ref.ranged->convertFrom0to1 (norm);
        const int steps = ref.rp->getNumSteps();
        if (steps > 1 && ref.rp->isDiscrete())
            return std::round (norm * (double) (steps - 1));
        return (double) norm;
    }

    // -------- JSON reply helpers ----------------------------------------------------------------
    template <typename Fill>
    static juce::String okReply (const juce::var& id, Fill&& fill)
    {
        auto* o = new juce::DynamicObject();
        o->setProperty ("ok", true);
        if (! id.isVoid()) o->setProperty ("id", id);
        fill (*o);
        return juce::JSON::toString (juce::var (o), true);   // compact, one line
    }

    static juce::String errorReply (const juce::String& err, const juce::var& id)
    {
        auto* o = new juce::DynamicObject();
        o->setProperty ("ok", false);
        o->setProperty ("error", err);
        if (! id.isVoid()) o->setProperty ("id", id);
        return juce::JSON::toString (juce::var (o), true);
    }

    juce::AudioProcessor&    processor;
    juce::MidiKeyboardState& keyboardState;
    const int                listenPort;

    juce::StreamingSocket listener;
    juce::StreamingSocket client;   // reserved for an explicit-drop handle (teardown)

    std::unordered_map<std::string, ParamRef> byId;

    static constexpr int kRingSize = 256;
    juce::AbstractFifo         fifo { kRingSize };
    std::array<Command, kRingSize> ring;
    std::array<bool,    kRingSize> wantAck { {} };
    juce::WaitableEvent        appliedEvent;

    // Full-state payloads. Written by the socket thread BEFORE it blocks on appliedEvent, read/written by the
    // message-thread drain, read by the socket thread AFTER the wait - the wait/signal pair provides the
    // happens-before, and the protocol is serial (one client, one in-flight request), so no lock is needed.
    std::string pendingLoadXml;   // load_state: the XML to apply
    std::string fetchedStateXml;  // get_full_state: the snapshot to return
    bool        lastLoadOk = false;

    std::atomic<Status> status { Status::Idle };

    JUCE_DECLARE_NON_COPYABLE_WITH_LEAK_DETECTOR (ControlListener)
};

} // namespace sidechain
