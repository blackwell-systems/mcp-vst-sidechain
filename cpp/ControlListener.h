#pragma once
#include <juce_audio_processors/juce_audio_processors.h>
#include <juce_audio_basics/juce_audio_basics.h>
#include <algorithm>
#include <atomic>
#include <cmath>
#include <deque>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <unordered_map>
#include <vector>

// ================================================================================================
// sidechain::ControlListener - an in-process LIVE-CONTROL listener for any juce::AudioProcessor.
//
// Point it at a processor (your own plugin, or a child plugin instance loaded via
// juce::AudioPluginFormatManager - see Host.h) and external agents can drive it in real time over a
// localhost socket: move any parameter, play notes, ask for a parameter's current value, and get/set the
// opaque full-state blob. The Go MCP server (this repo) is a thin forwarder that speaks the wire protocol
// below 1:1.
//
// THREAD MODEL (the load-bearing safety rules; see docs/CONCURRENCY.md):
//   • The AUDIO thread is untouched: this class never blocks or allocates on it. Reads are parameter atomics.
//   • MANY SOCKET threads (one per connected controller) parse JSON. For a READ they snapshot the parameter's
//     atomic and reply directly (safe from any thread). For a MUTATION they push a Command carrying a per-request
//     Completion into an MPSC queue and fire an AsyncUpdate, then wait (bounded) on their OWN Completion.
//   • The MESSAGE THREAD is the SINGLE applier: it drains the queue (this : juce::AsyncUpdater) and applies each
//     Command via setValueNotifyingHost / keyboardState / setStateInformation, then signals that command's
//     Completion. Every mutation serializes here regardless of how many controllers enqueued.
//   The queue lock lives on the control path (tens of ms), never the audio thread, so it is free to be a lock.
//
// TRANSPORT - localhost TCP, line-delimited JSON, request/response. Bound to 127.0.0.1 ONLY (never a routable
// interface). MULTIPLE clients may connect at once; each gets a clientID at the ping handshake. Commands mirror
// the MCP tools 1:1:
//   ping · get_param{param} · set_param{param,(value|normalized|choice)} · note_on{chan,note,vel} ·
//   note_off{chan,note} · all_notes_off · reset_init · load_state{state} · get_full_state · get_state.
//   (load_state/get_full_state carry `state`: base64 of the plugin's own opaque getStateInformation bytes.)
// ================================================================================================

namespace sidechain
{

class ControlListener : private juce::Thread,
                        private juce::AsyncUpdater
{
public:
    // Session status. Ordered idle < listening < connected (connected = at least one controller attached).
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
        listener.close();     // unblocks waitForNextConnection() so no new clients are accepted
        {
            // Close every live connection to unblock its handler's blocked read.
            std::lock_guard<std::mutex> lk (clientsMutex);
            for (auto& c : clients) c.socket->close();
        }
        stopThread (2000);    // join the accept thread
        cancelPendingUpdate();
        // The handler threads are detached; wait (bounded) for them to finish so none touches *this afterwards.
        for (int i = 0; i < 300 && liveHandlers.load() > 0; ++i)
            juce::Thread::sleep (10);
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

    // Per-request completion. Heap-allocated and shared between the requesting socket handler and the message
    // thread, so a bounded timeout on the handler can never free it out from under a late apply. The handler only
    // reads the result fields AFTER event.wait() returns true (the signal provides the happens-before).
    struct Completion
    {
        juce::WaitableEvent event;       // auto-reset; a single waiter (the requesting handler)
        bool        loadOk  = false;     // LoadState: applied ok
        bool        stateOk = false;     // GetFullState: getStateInformation ran
        std::string state;               // GetFullState: base64 snapshot
    };

    enum class Kind : uint8_t { SetParam, NoteOn, NoteOff, AllNotesOff, ResetInit, LoadState, GetFullState };
    struct Command
    {
        Kind  kind = Kind::SetParam;
        int   a = 0;        // SetParam: param index · NoteOn/Off: channel
        int   b = 0;        // NoteOn/Off: note number
        float f = 0.0f;     // SetParam: normalized value · NoteOn: velocity (0..1)
        std::string                 payload;    // LoadState: the base64 opaque blob
        std::shared_ptr<Completion> done;       // null => fire-and-forget (no ack wait)
        int   clientId = 0;                      // originating controller (attribution; used by C2 change events)
    };

    // -------- socket accept loop (the juce::Thread) ---------------------------------------------
    void run() override
    {
        // Bind 127.0.0.1 ONLY. createListener with an explicit local host never exposes a routable iface.
        if (! listener.createListener (listenPort, "127.0.0.1"))
        {
            status.store (Status::Idle);
            return;
        }
        status.store (Status::Listening);

        while (! threadShouldExit())
        {
            std::unique_ptr<juce::StreamingSocket> conn (listener.waitForNextConnection());
            if (conn == nullptr || threadShouldExit())
                break;   // listener closed (shutdown) or fatal error

            const int cid = ++nextClientId;
            {
                std::lock_guard<std::mutex> lk (clientsMutex);
                clients.push_back ({ conn.get(), cid });   // raw ptr for teardown; the handler owns the socket
            }
            liveHandlers.fetch_add (1);
            status.store (Status::Connected);

            // One handler thread per controller. Detached, so a controller that lingers does not hold up accept;
            // the destructor waits on liveHandlers instead of joining.
            juce::StreamingSocket* raw = conn.release();
            std::thread ([this, raw, cid]
            {
                std::unique_ptr<juce::StreamingSocket> owned (raw);
                serveClient (*owned, cid);
                removeClient (cid);
                liveHandlers.fetch_sub (1);
            }).detach();
        }
    }

    void removeClient (int cid)
    {
        std::lock_guard<std::mutex> lk (clientsMutex);
        clients.erase (std::remove_if (clients.begin(), clients.end(),
                                       [cid] (const Conn& c) { return c.id == cid; }),
                       clients.end());
        if (clients.empty() && ! threadShouldExit())
            status.store (Status::Listening);
    }

    void serveClient (juce::StreamingSocket& conn, int clientId)
    {
        std::string inbox;
        char buf[2048];

        while (! threadShouldExit())
        {
            // Wait for readability first, THEN read, so a graceful peer close (ready + 0 bytes = EOF) is
            // distinguished from "no data yet" and the handler returns instead of spinning.
            const int ready = conn.waitUntilReady (true, 200);
            if (ready < 0) break;    // socket error
            if (ready == 0) continue; // timed out with nothing to read: re-check shutdown, wait again

            const int got = conn.read (buf, (int) sizeof (buf), false);
            if (got <= 0) break;     // ready but 0 bytes => peer closed (EOF); negative => error

            inbox.append (buf, (size_t) got);
            for (auto nl = inbox.find ('\n'); nl != std::string::npos; nl = inbox.find ('\n'))
            {
                const std::string line = inbox.substr (0, nl);
                inbox.erase (0, nl + 1);
                if (line.find_first_not_of (" \t\r") == std::string::npos)
                    continue;

                const juce::String reply = handleLine (line, clientId);
                const juce::String out   = reply + "\n";
                if (conn.write (out.toRawUTF8(), (int) out.getNumBytesAsUTF8()) < 0)
                    return;
            }
        }
    }

    // Parse one JSON request line and produce one JSON reply line (a socket handler thread).
    juce::String handleLine (const std::string& line, int clientId)
    {
        juce::var req = juce::JSON::parse (juce::String (juce::CharPointer_UTF8 (line.c_str())));
        if (! req.isObject())
            return errorReply ("bad_json", {});

        const juce::String cmd = req.getProperty ("cmd", juce::var()).toString();
        const juce::var    id  = req.getProperty ("id", juce::var());   // request correlation id (echoed back)

        if (cmd == "ping")
            return okReply (id, [clientId] (juce::DynamicObject& o)
            {
                o.setProperty ("pong",   true);
                o.setProperty ("client", clientId);   // controller identity, assigned by the host
            });

        if (cmd == "get_param")
            return handleGetParam (req, id);

        if (cmd == "set_param")
            return handleSetParam (req, id, clientId);

        if (cmd == "note_on" || cmd == "note_off")
            return handleNote (req, id, cmd == "note_on", clientId);

        if (cmd == "all_notes_off")
        {
            Command c; c.kind = Kind::AllNotesOff; c.clientId = clientId;
            enqueue (std::move (c));   // fire-and-forget
            return okReply (id, [] (juce::DynamicObject&) {});
        }

        if (cmd == "reset_init")   // recall an init/default patch (host-supplied), message-thread
        {
            Command c; c.kind = Kind::ResetInit; c.clientId = clientId;
            if (submit (std::move (c), 500) == nullptr)
                return errorReply ("timeout", id);
            return okReply (id, [] (juce::DynamicObject& o) { o.setProperty ("reset", true); });
        }

        if (cmd == "load_state")   // push a WHOLE patch (the plugin's opaque state blob) into the live instance
        {
            // `state` is base64 of the exact bytes the plugin's own getStateInformation produced. No format
            // assumption (XML-wrapped or raw binary): the bridge round-trips opaque bytes.
            std::string blob = req.getProperty ("state", juce::var()).toString().toStdString();
            if (blob.empty())
                return errorReply ("no_state", id);
            Command c; c.kind = Kind::LoadState; c.payload = std::move (blob); c.clientId = clientId;
            auto done = submit (std::move (c), 2000);   // a full-state restore can be heavier than a param set
            if (done == nullptr)
                return errorReply ("timeout", id);
            return done->loadOk ? okReply (id, [] (juce::DynamicObject& o) { o.setProperty ("loaded", true); })
                                : errorReply ("load_failed", id);
        }

        if (cmd == "get_full_state")   // pull the WHOLE live patch as the plugin's opaque state blob (base64)
        {
            Command c; c.kind = Kind::GetFullState; c.clientId = clientId;
            auto done = submit (std::move (c), 2000);
            if (done == nullptr || ! done->stateOk)
                return errorReply ("state_unavailable", id);
            // An empty state (a plugin with no persistent state) is a valid opaque blob; stateOk distinguishes it
            // from a failure to run getStateInformation.
            return okReply (id, [&] (juce::DynamicObject& o) { o.setProperty ("state", juce::String (done->state)); });
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

    juce::String handleSetParam (const juce::var& req, const juce::var& id, int clientId)
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

        // Queue for the message thread and wait (bounded) on this request's own completion, so the ack means
        // "applied" - clean request/response semantics even with other controllers driving concurrently.
        Command c; c.kind = Kind::SetParam; c.a = it->second.index; c.f = norm; c.clientId = clientId;
        if (submit (std::move (c), 250) == nullptr)
            return errorReply ("timeout", id);

        const float applied = rp->getValue();   // atomic; the completion guarantees the set has applied
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("param",      juce::String (pid));
            o.setProperty ("normalized", applied);
            o.setProperty ("value",      valueForCatalog (it->second, applied));
            o.setProperty ("text",       rp->getText (applied, 256));
        });
    }

    juce::String handleNote (const juce::var& req, const juce::var& id, bool on, int clientId)
    {
        const int   chan = juce::jlimit (1, 16,  (int)   req.getProperty ("chan", 1));
        const int   note = juce::jlimit (0, 127, (int)   req.getProperty ("note", 60));
        const float vel  = juce::jlimit (0.0f, 1.0f, (float) req.getProperty ("vel", 0.8));
        Command c; c.kind = on ? Kind::NoteOn : Kind::NoteOff; c.a = chan; c.b = note; c.f = vel; c.clientId = clientId;
        enqueue (std::move (c));   // fire-and-forget (best-effort under load)
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("note", note);
            o.setProperty ("on",   on);
        });
    }

    // Session snapshot: every resolved param's current value (id → normalized/value). Reads only (atomic
    // getValue), so it is safe from a socket handler thread.
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

    // -------- MPSC command queue (many socket handlers produce, the message thread is the single consumer) ----
    // A short lock here is fine: it is the control path (tens of ms), never the audio thread.
    bool enqueue (Command&& c)
    {
        {
            std::lock_guard<std::mutex> lk (queueMutex);
            if (queue.size() >= kMaxQueue)
                return false;   // backpressure: reject rather than grow unbounded (a wedged controller cannot pile up)
            queue.push_back (std::move (c));
        }
        triggerAsyncUpdate();
        return true;
    }

    // Enqueue a command carrying a fresh Completion and wait (bounded) for the message thread to apply+signal it.
    // Returns the Completion (safe to read) on success, or nullptr if the queue was full or the wait timed out.
    std::shared_ptr<Completion> submit (Command&& c, int timeoutMs)
    {
        auto done = std::make_shared<Completion>();
        c.done = done;
        if (! enqueue (std::move (c)))
            return nullptr;
        return done->event.wait (timeoutMs) ? done : nullptr;
    }

    void handleAsyncUpdate() override
    {
        std::deque<Command> local;
        {
            std::lock_guard<std::mutex> lk (queueMutex);
            local.swap (queue);
        }
        for (auto& c : local)
        {
            applyCommand (c);
            if (c.done)
                c.done->event.signal();   // wakes the requesting handler; provides happens-before for done->*
        }
    }

    void applyCommand (Command& c)
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
                // Base64-decode the opaque blob back to the EXACT bytes the plugin emitted, then apply it via the
                // normal host path. No format assumption (works for XML-wrapped AND raw-binary state); byte-exact.
                {
                    juce::MemoryOutputStream decoded;
                    if (juce::Base64::convertFromBase64 (decoded, juce::String (c.payload)) && decoded.getDataSize() > 0)
                    {
                        processor.setStateInformation (decoded.getData(), (int) decoded.getDataSize());
                        if (c.done) c.done->loadOk = true;
                    }
                }
                break;
            case Kind::GetFullState:
                // Snapshot the live patch as the plugin's own opaque bytes and base64 them verbatim.
                {
                    juce::MemoryBlock mb;
                    processor.getStateInformation (mb);
                    if (c.done)
                    {
                        c.done->state   = juce::Base64::toBase64 (mb.getData(), mb.getSize()).toStdString();
                        c.done->stateOk = true;
                    }
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

    std::unordered_map<std::string, ParamRef> byId;

    // MPSC command queue: many socket handlers produce, the message thread is the single consumer/applier.
    std::mutex                queueMutex;
    std::deque<Command>       queue;
    static constexpr size_t   kMaxQueue = 4096;

    // Connected controllers. Handlers own their sockets; the registry keeps a raw pointer so the destructor can
    // close each socket to unblock a blocked read. liveHandlers lets the destructor wait for detached handlers.
    struct Conn { juce::StreamingSocket* socket; int id; };
    std::mutex                clientsMutex;
    std::vector<Conn>         clients;
    std::atomic<int>          liveHandlers { 0 };
    std::atomic<int>          nextClientId { 0 };

    std::atomic<Status> status { Status::Idle };

    JUCE_DECLARE_NON_COPYABLE_WITH_LEAK_DETECTOR (ControlListener)
};

} // namespace sidechain
