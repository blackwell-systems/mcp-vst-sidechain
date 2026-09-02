#pragma once
#include <juce_core/juce_core.h>
#include <juce_events/juce_events.h>
#include <algorithm>
#include <atomic>
#include <deque>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>
#include "GovernedState.h"
#include "PluginBridge.h"

// ================================================================================================
// sidechain::ControlServer - the VST-agnostic LIVE-CONTROL plane. It owns the transport, the wire protocol,
// controller identity, the command queue, change-event broadcast, and the conflict tier. It drives a plugin
// only through a PluginBridge (see PluginBridge.h), so it has NO dependency on juce_audio_processors or any
// concrete plugin/parameter/state type: point it at any PluginBridge and external agents can drive that thing
// over a localhost socket. The Go MCP server (this repo) is a thin forwarder that speaks the protocol below.
//
// (This is the former ControlListener, split so the transport/protocol/identity/event machinery is reusable
// beyond a JUCE plugin; JucePluginBridge holds everything plugin-specific.)
//
// THREAD MODEL (the load-bearing safety rules; see docs/CONCURRENCY.md):
//   • The AUDIO thread is untouched: this class never blocks or allocates on it. Bridge reads are atomics.
//   • MANY SOCKET threads (one per connected controller) parse JSON. A READ asks the bridge (safe from any
//     thread) and replies directly. A MUTATION pushes a Command carrying a per-request Completion into an MPSC
//     queue and fires an AsyncUpdate, then waits (bounded) on its OWN Completion. Each connection also has an
//     OUTBOUND queue; its handler is the SINGLE writer to that socket (replies + broadcast events), so writes
//     never race.
//   • The MESSAGE THREAD is the SINGLE applier: it drains the command queue and applies each Command via the
//     bridge, then signals that command's Completion. Every mutation serializes here (this is the conflict tier:
//     last-writer-wins by message-thread order; a governed-convergence engine (gsm) can slot in here at C3).
//   • CHANGE EVENTS: the bridge calls onParamChanged(index) when any parameter changes (by any controller, the
//     plugin's editor, or host automation). That may fire on any thread, so we broadcast directly only when on
//     the message thread (with attribution); otherwise we mark the param dirty and defer to the drain, keeping
//     the audio path lock-free.
//
// TRANSPORT - localhost TCP, line-delimited JSON. Bound to 127.0.0.1 ONLY. MANY clients may connect at once;
// each gets a clientID at the ping handshake. The server also PUSHES unsolicited `param_changed` events, so the
// protocol is multiplexed async (a reply carries the request `id`; an event carries an `event` field). Requests:
//   ping · get_param{param} · set_param{param,(value|normalized|choice)} · note_on{chan,note,vel} ·
//   note_off{chan,note} · all_notes_off · reset_init · load_state{state} · get_full_state · get_state ·
//   render{note,velocity,channel,gate_ms,duration_ms,input_kind,input_freq,input_level} ·
//   govern{op:(acquire_instance|release_instance|acquire_section{group}|release_section{group})} · get_governed.
//   (load_state/get_full_state carry `state`: base64 of the plugin's own opaque state bytes, handled by the bridge.
//    govern applies a command to the C3 governed coordination state (hierarchical edit leases + patch generation)
//    on the single applier and pushes a `governed_changed` event; a whole-patch change bumps the generation and a
//    disconnect frees the departing controller's leases. See GovernedState.h and docs/CONCURRENCY.md.)
// ================================================================================================

namespace sidechain
{

class ControlServer : private juce::Thread,
                      private juce::AsyncUpdater,
                      private ParamChangeSink
{
public:
    // Session status. Ordered idle < listening < connected (connected = at least one controller attached).
    enum class Status { Idle, Listening, Connected };

    static constexpr int kDefaultPort = 51703;   // 127.0.0.1:51703 (the Go server's connect_live default)

    ControlServer (PluginBridge& b, int port = kDefaultPort)
        : juce::Thread ("sidechain-control"), bridge (b), listenPort (port)
    {
        dirtyCount = bridge.paramCount();
        dirty = std::make_unique<std::atomic<bool>[]> ((size_t) juce::jmax (1, dirtyCount));
        leasableSections = bridge.sectionGroups();   // the param groups a controller may take a section lease on
        bridge.setChangeSink (this);   // start hearing parameter changes

        status.store (Status::Listening);
        startThread (juce::Thread::Priority::normal);
    }

    ~ControlServer() override
    {
        bridge.setChangeSink (nullptr);   // stop change callbacks before we tear down
        signalThreadShouldExit();
        listener.close();     // unblocks waitForNextConnection() so no new clients are accepted
        {
            std::lock_guard<std::mutex> lk (clientsMutex);
            for (auto& c : clients) c.socket->close();   // unblock each handler's blocked read
        }
        stopThread (2000);    // join the accept thread
        cancelPendingUpdate();
        for (int i = 0; i < 300 && liveHandlers.load() > 0; ++i)   // wait (bounded) for detached handlers
            juce::Thread::sleep (10);
        status.store (Status::Idle);
    }

    Status getStatus() const noexcept { return status.load(); }
    int     getPort()   const noexcept { return listenPort; }

private:
    // Per-request completion. Heap-allocated and shared between the requesting handler and the message thread, so
    // a bounded timeout can never free it under a late apply. Result fields read only AFTER event.wait() is true.
    struct Completion
    {
        juce::WaitableEvent event;       // auto-reset; a single waiter (the requesting handler)
        bool        loadOk  = false;
        bool        stateOk = false;
        std::string state;
        Resolution  govRes  = Resolution::Applied;   // Govern: how the conflict tier resolved the command
        GovState    govState;                        // Govern / GetGoverned: the governed state after the command
        Measurement measurement;                     // Render: the measured output of the offline render
    };

    enum class Kind : uint8_t { SetParam, NoteOn, NoteOff, AllNotesOff, ResetInit, LoadState, GetFullState,
                                Govern, GetGoverned, Render };
    struct Command
    {
        Kind  kind = Kind::SetParam;
        int   a = 0;
        int   b = 0;
        float f = 0.0f;
        std::string                 payload;    // LoadState: the base64 opaque blob
        GovCmd                      gov;        // Govern: the governed command
        RenderSpec                  render;     // Render: the offline-render spec
        std::shared_ptr<Completion> done;       // null => fire-and-forget
        int   clientId = 0;                      // originating controller (attribution for change events)
    };

    // Per-connection outbound queue. The connection's handler is the ONLY writer to its socket; the broadcaster
    // and the handler push here, the handler drains and writes. Bounded: drop oldest (events are latest-value
    // snapshots, so dropping a stale one is fine; a reply is pushed then flushed immediately, so it is not lost).
    struct Outbound
    {
        std::mutex mu;
        std::deque<std::string> q;
        static constexpr size_t kMax = 512;
        void push (std::string line)
        {
            std::lock_guard<std::mutex> lk (mu);
            while (q.size() >= kMax) q.pop_front();
            q.push_back (std::move (line));
        }
        std::deque<std::string> drain()
        {
            std::deque<std::string> out;
            std::lock_guard<std::mutex> lk (mu);
            out.swap (q);
            return out;
        }
    };
    struct Conn { juce::StreamingSocket* socket; int id; std::shared_ptr<Outbound> out; };

    // -------- socket accept loop (the juce::Thread) ---------------------------------------------
    void run() override
    {
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
                break;

            const int cid = ++nextClientId;
            auto out = std::make_shared<Outbound>();
            {
                std::lock_guard<std::mutex> lk (clientsMutex);
                clients.push_back ({ conn.get(), cid, out });
            }
            liveHandlers.fetch_add (1);
            status.store (Status::Connected);

            juce::StreamingSocket* raw = conn.release();
            std::thread ([this, raw, cid, out]
            {
                std::unique_ptr<juce::StreamingSocket> owned (raw);
                serveClient (*owned, cid, *out);
                removeClient (cid);
                if (! threadShouldExit())
                {
                    // Free any leases the departing controller held (disconnect cleanup). Fire-and-forget onto the
                    // message thread; skipped during teardown, where the drain is already stopping.
                    Command gone; gone.kind = Kind::Govern;
                    gone.gov = GovCmd { GovOp::ControllerGone, cid, {} };
                    gone.clientId = cid;
                    enqueue (std::move (gone));
                }
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

    bool flushOutbound (juce::StreamingSocket& conn, Outbound& out)
    {
        for (auto& line : out.drain())
            if (conn.write (line.data(), (int) line.size()) < 0)
                return false;
        return true;
    }

    void serveClient (juce::StreamingSocket& conn, int clientId, Outbound& out)
    {
        std::string inbox;
        char buf[2048];

        while (! threadShouldExit())
        {
            if (! flushOutbound (conn, out)) return;   // push queued events (and any pending replies)

            const int ready = conn.waitUntilReady (true, 50);
            if (ready < 0) return;    // socket error
            if (ready == 0) continue; // timed out: loop to drain events + re-read

            const int got = conn.read (buf, (int) sizeof (buf), false);
            if (got <= 0) return;     // ready but 0 bytes => peer closed (EOF); negative => error

            inbox.append (buf, (size_t) got);
            bool haveReply = false;
            for (auto nl = inbox.find ('\n'); nl != std::string::npos; nl = inbox.find ('\n'))
            {
                const std::string line = inbox.substr (0, nl);
                inbox.erase (0, nl + 1);
                if (line.find_first_not_of (" \t\r") == std::string::npos)
                    continue;
                out.push (handleLine (line, clientId).toStdString() + "\n");
                haveReply = true;
            }
            if (haveReply && ! flushOutbound (conn, out)) return;   // flush replies promptly
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
                o.setProperty ("client", clientId);
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
            enqueue (std::move (c));
            return okReply (id, [] (juce::DynamicObject&) {});
        }

        if (cmd == "reset_init")
        {
            Command c; c.kind = Kind::ResetInit; c.clientId = clientId;
            if (submit (std::move (c), 500) == nullptr)
                return errorReply ("timeout", id);
            return okReply (id, [] (juce::DynamicObject& o) { o.setProperty ("reset", true); });
        }

        if (cmd == "load_state")
        {
            std::string blob = req.getProperty ("state", juce::var()).toString().toStdString();
            if (blob.empty())
                return errorReply ("no_state", id);
            Command c; c.kind = Kind::LoadState; c.payload = std::move (blob); c.clientId = clientId;
            auto done = submit (std::move (c), 2000);
            if (done == nullptr)
                return errorReply ("timeout", id);
            return done->loadOk ? okReply (id, [] (juce::DynamicObject& o) { o.setProperty ("loaded", true); })
                                : errorReply ("load_failed", id);
        }

        if (cmd == "get_full_state")
        {
            Command c; c.kind = Kind::GetFullState; c.clientId = clientId;
            auto done = submit (std::move (c), 2000);
            if (done == nullptr || ! done->stateOk)
                return errorReply ("state_unavailable", id);
            return okReply (id, [&] (juce::DynamicObject& o) { o.setProperty ("state", juce::String (done->state)); });
        }

        if (cmd == "get_state")
            return handleGetState (id);

        if (cmd == "render")
            return handleRender (req, id, clientId);

        if (cmd == "govern")
            return handleGovern (req, id, clientId);

        if (cmd == "get_governed")
            return handleGetGoverned (id);

        return errorReply ("unknown_cmd", id);
    }

    juce::String handleGetParam (const juce::var& req, const juce::var& id)
    {
        const std::string pid = req.getProperty ("param", juce::var()).toString().toStdString();
        ParamValue pv;
        if (! bridge.readParam (pid, pv))
            return errorReply ("unknown_param", id);
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("param",      juce::String (pid));
            o.setProperty ("value",      pv.value);
            o.setProperty ("normalized", pv.normalized);
            o.setProperty ("text",       juce::String (pv.text));
        });
    }

    juce::String handleSetParam (const juce::var& req, const juce::var& id, int clientId)
    {
        const std::string pid = req.getProperty ("param", juce::var()).toString().toStdString();

        SetForm form;
        double  v = 0.0;
        if (req.hasProperty ("normalized"))      { form = SetForm::Normalized; v = (double) req.getProperty ("normalized", 0.0); }
        else if (req.hasProperty ("choice"))     { form = SetForm::Choice;     v = (double) req.getProperty ("choice", 0);       }
        else if (req.hasProperty ("value"))      { form = SetForm::Value;      v = (double) req.getProperty ("value", 0.0);      }
        else
            return errorReply ("no_value", id);

        int   index = -1;
        float norm  = 0.0f;
        if (! bridge.resolveSet (pid, form, v, index, norm))
            return errorReply ("unknown_param", id);

        Command c; c.kind = Kind::SetParam; c.a = index; c.f = norm; c.clientId = clientId;
        if (submit (std::move (c), 250) == nullptr)
            return errorReply ("timeout", id);

        ParamValue pv;
        bridge.readParam (pid, pv);   // read back the applied value for the reply
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("param",      juce::String (pid));
            o.setProperty ("normalized", pv.normalized);
            o.setProperty ("value",      pv.value);
            o.setProperty ("text",       juce::String (pv.text));
        });
    }

    juce::String handleNote (const juce::var& req, const juce::var& id, bool on, int clientId)
    {
        const int   chan = juce::jlimit (1, 16,  (int)   req.getProperty ("chan", 1));
        const int   note = juce::jlimit (0, 127, (int)   req.getProperty ("note", 60));
        const float vel  = juce::jlimit (0.0f, 1.0f, (float) req.getProperty ("vel", 0.8));
        Command c; c.kind = on ? Kind::NoteOn : Kind::NoteOff; c.a = chan; c.b = note; c.f = vel; c.clientId = clientId;
        enqueue (std::move (c));
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("note", note);
            o.setProperty ("on",   on);
        });
    }

    juce::String handleGetState (const juce::var& id)
    {
        auto* params = new juce::DynamicObject();
        const auto all = bridge.snapshotAll();
        for (const auto& kv : all)
        {
            auto* one = new juce::DynamicObject();
            one->setProperty ("value",      kv.second.value);
            one->setProperty ("normalized", kv.second.normalized);
            params->setProperty (juce::String (kv.first), juce::var (one));
        }
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("count",  (int) all.size());
            o.setProperty ("params", juce::var (params));
        });
    }

    // -------- offline render + measure (Tier 1 + 2; see docs/RENDER-SCOPING.md) ------------------
    // A render is a bounded BLOCKING command on the single applier thread (like get_full_state): the bridge
    // resets the processor's DSP state, drives it with a MIDI note (note-driven) or an input signal (effect), loops
    // processBlock over the duration, and analyzes the output. It serializes with param sets, so it introduces no
    // new concurrency hazard. All request fields are optional and carry the RenderSpec defaults; the host
    // auto-detects instrument vs effect. The reply carries the measurement object.
    juce::String handleRender (const juce::var& req, const juce::var& id, int clientId)
    {
        RenderSpec spec;   // defaults; each field is overridden only if present in the request
        if (req.hasProperty ("note"))        spec.note       = (int)   req.getProperty ("note", spec.note);
        if (req.hasProperty ("velocity"))    spec.velocity   = (float) (double) req.getProperty ("velocity", spec.velocity);
        if (req.hasProperty ("channel"))     spec.channel    = (int)   req.getProperty ("channel", spec.channel);
        if (req.hasProperty ("gate_ms"))     spec.gateMs     = (int)   req.getProperty ("gate_ms", spec.gateMs);
        if (req.hasProperty ("duration_ms")) spec.durationMs = (int)   req.getProperty ("duration_ms", spec.durationMs);
        if (req.hasProperty ("input_freq"))  spec.inputFreq  = (double) req.getProperty ("input_freq", spec.inputFreq);
        if (req.hasProperty ("input_level")) spec.inputLevel = (float) (double) req.getProperty ("input_level", spec.inputLevel);
        if (req.hasProperty ("input_kind"))
        {
            const juce::String k = req.getProperty ("input_kind", juce::var()).toString();
            if      (k == "silence") spec.inputKind = InputKind::Silence;
            else if (k == "sine")    spec.inputKind = InputKind::Sine;
            else if (k == "noise")   spec.inputKind = InputKind::Noise;
            else if (k == "impulse") spec.inputKind = InputKind::Impulse;
            else return errorReply ("bad_input_kind", id);
        }

        Command c; c.kind = Kind::Render; c.render = spec; c.clientId = clientId;
        auto done = submit (std::move (c), 5000);   // generous: a render is bounded but not instant
        if (done == nullptr)
            return errorReply ("timeout", id);
        if (! done->measurement.ok)
            return errorReply ("render_failed", id);
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("measurement", measurementToVar (done->measurement));
        });
    }

    static juce::var measurementToVar (const Measurement& m)
    {
        auto* bands = new juce::DynamicObject();
        bands->setProperty ("low_db",  m.lowDb);
        bands->setProperty ("mid_db",  m.midDb);
        bands->setProperty ("high_db", m.highDb);
        auto* o = new juce::DynamicObject();
        o->setProperty ("duration_sec", m.durationSec);
        o->setProperty ("sample_rate",  m.sampleRate);
        o->setProperty ("channels",     m.channels);
        o->setProperty ("peak_db",      m.peakDb);
        o->setProperty ("rms_db",       m.rmsDb);
        o->setProperty ("crest",        m.crest);
        o->setProperty ("centroid_hz",  m.centroidHz);
        o->setProperty ("bands",        juce::var (bands));
        o->setProperty ("silent",       m.silent);
        o->setProperty ("clipped",      m.clipped);
        return juce::var (o);
    }

    // -------- governed coordination state (C3 conflict tier; see GovernedState.h / docs/CONCURRENCY.md) ---------
    // A governed command is a MUTATION, so it flows through the same MPSC queue as set_param and applies on the
    // single message thread via GovState::apply. The reply carries the resolution (applied/compensated/rejected)
    // and the resulting governed state. Lease ops attribute to the originating controller (by = clientId).
    juce::String handleGovern (const juce::var& req, const juce::var& id, int clientId)
    {
        const juce::String op = req.getProperty ("op", juce::var()).toString();
        GovCmd g; g.by = clientId;   // a lease is acquired/released AS the calling controller
        if      (op == "acquire_instance") g.op = GovOp::AcquireInstance;
        else if (op == "release_instance") g.op = GovOp::ReleaseInstance;
        else if (op == "acquire_section" || op == "release_section")
        {
            const juce::String grp = req.getProperty ("group", juce::var()).toString();
            if (! isLeasableSection (grp))
                return errorReply ("unknown_section", id);   // section leases bind to the plugin's param groups
            g.op = (op == "acquire_section") ? GovOp::AcquireSection : GovOp::ReleaseSection;
            g.section = grp.toStdString();
        }
        else return errorReply ("unknown_op", id);   // BumpGeneration / ControllerGone are internal, not wire-reachable

        Command c; c.kind = Kind::Govern; c.gov = g; c.clientId = clientId;
        auto done = submit (std::move (c), 250);
        if (done == nullptr)
            return errorReply ("timeout", id);
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("resolution", juce::String (resStr (done->govRes)));   // a rejection is an ok reply, not an error
            o.setProperty ("governed",   govToVar (done->govState));
        });
    }

    juce::String handleGetGoverned (const juce::var& id)
    {
        Command c; c.kind = Kind::GetGoverned;
        auto done = submit (std::move (c), 250);
        if (done == nullptr || ! done->stateOk)
            return errorReply ("timeout", id);
        return okReply (id, [&] (juce::DynamicObject& o)
        {
            o.setProperty ("governed", govToVar (done->govState));
            juce::Array<juce::var> secs;                       // the leasable param-group names, so an agent can discover them
            for (const auto& s : leasableSections) secs.add (juce::String (s));
            o.setProperty ("sections", secs);
        });
    }

    bool isLeasableSection (const juce::String& grp) const
    {
        const std::string s = grp.toStdString();
        return std::find (leasableSections.begin(), leasableSections.end(), s) != leasableSections.end();
    }

    static const char* resStr (Resolution r) noexcept
    {
        switch (r)
        {
            case Resolution::Applied:     return "applied";
            case Resolution::Compensated: return "compensated";
            case Resolution::Rejected:    return "rejected";
        }
        return "applied";
    }
    static juce::var govToVar (const GovState& g)
    {
        auto* secs = new juce::DynamicObject();          // held section leases only: { "<group>": <holder> }
        for (const auto& kv : g.sectionLease)
            secs->setProperty (juce::String (kv.first), kv.second);
        auto* o = new juce::DynamicObject();
        o->setProperty ("instance_lease", g.soloInstanceLease);
        o->setProperty ("section_leases", juce::var (secs));
        o->setProperty ("generation",     g.generation);
        return juce::var (o);
    }

    void broadcastGovernedChanged (int by, Resolution res)
    {
        auto* o = new juce::DynamicObject();
        o->setProperty ("event",      "governed_changed");
        o->setProperty ("governed",   govToVar (governed));
        o->setProperty ("resolution", juce::String (resStr (res)));
        o->setProperty ("by",         by);
        const std::string line = juce::JSON::toString (juce::var (o), true).toStdString() + "\n";
        std::lock_guard<std::mutex> lk (clientsMutex);
        for (auto& c : clients) c.out->push (line);
    }

    // -------- change events (this : ParamChangeSink; the bridge calls us) -----------------------
    void onParamChanged (int index) override
    {
        // Fires for ANY change (our command, the plugin's editor, host automation) and "may be called from any
        // thread". Never lock on a non-message thread (could be the audio thread): broadcast now only if on the
        // message thread (with attribution); otherwise mark dirty and defer to the drain.
        if (juce::MessageManager::existsAndIsCurrentThread())
            broadcastParamChanged (index, applyingClientId.load());
        else if (index >= 0 && index < dirtyCount)
        {
            dirty[(size_t) index].store (true, std::memory_order_release);
            triggerAsyncUpdate();
        }
    }

    void broadcastParamChanged (int index, int by)
    {
        std::string pid;
        ParamValue  pv;
        if (! bridge.describeIndex (index, pid, pv))
            return;
        auto* o = new juce::DynamicObject();
        o->setProperty ("event",      "param_changed");
        o->setProperty ("param",      juce::String (pid));
        o->setProperty ("normalized", pv.normalized);
        o->setProperty ("value",      pv.value);
        o->setProperty ("text",       juce::String (pv.text));
        o->setProperty ("by",         by);   // originating clientID when known (message-thread path), else 0
        const std::string line = juce::JSON::toString (juce::var (o), true).toStdString() + "\n";
        std::lock_guard<std::mutex> lk (clientsMutex);
        for (auto& c : clients) c.out->push (line);
    }

    // -------- MPSC command queue (many socket handlers produce, the message thread applies) -----
    bool enqueue (Command&& c)
    {
        {
            std::lock_guard<std::mutex> lk (queueMutex);
            if (queue.size() >= kMaxQueue)
                return false;
            queue.push_back (std::move (c));
        }
        triggerAsyncUpdate();
        return true;
    }

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
                c.done->event.signal();
        }
        // Broadcast parameter changes that fired OFF the message thread (deferred to keep the audio path
        // lock-free). Attribution is unavailable off-thread, so by = 0 (external / plugin / host automation).
        for (int i = 0; i < dirtyCount; ++i)
            if (dirty[(size_t) i].exchange (false, std::memory_order_acq_rel))
                broadcastParamChanged (i, 0);
    }

    void applyCommand (Command& c)
    {
        switch (c.kind)
        {
            case Kind::SetParam:
                applyingClientId.store (c.clientId);   // attribute the resulting change event to this controller
                bridge.applyParam (c.a, c.f);
                applyingClientId.store (0);
                break;
            case Kind::NoteOn:      bridge.noteOn  (c.a, c.b, c.f); break;
            case Kind::NoteOff:     bridge.noteOff (c.a, c.b, c.f); break;
            case Kind::AllNotesOff: bridge.allNotesOff(); break;
            case Kind::ResetInit:
                bridge.resetInit();
                applyGoverned (GovCmd { GovOp::BumpGeneration, c.clientId, {} }, c.clientId);   // whole patch changed
                break;
            case Kind::LoadState:
                {
                    applyingClientId.store (c.clientId);
                    const bool okLoad = bridge.loadState (c.payload);
                    applyingClientId.store (0);
                    if (okLoad)
                    {
                        if (c.done) c.done->loadOk = true;
                        applyGoverned (GovCmd { GovOp::BumpGeneration, c.clientId, {} }, c.clientId);   // whole patch changed
                    }
                }
                break;
            case Kind::GetFullState:
                if (c.done)
                    c.done->stateOk = bridge.saveState (c.done->state);
                break;
            case Kind::Govern:
                {
                    // The conflict tier, on the single applier thread. LWW still governs the continuous params
                    // (bridge.applyParam); this governs only the small invariant-bearing coordination state.
                    const Resolution res = applyGoverned (c.gov, c.clientId);
                    if (c.done)
                    {
                        c.done->govRes   = res;
                        c.done->govState = governed;
                    }
                }
                break;
            case Kind::GetGoverned:
                if (c.done)
                {
                    c.done->govState = governed;
                    c.done->stateOk  = true;
                }
                break;
            case Kind::Render:
                // Bounded blocking render on the single applier: reset, drive, processBlock loop, analyze.
                // Serializes with param sets (no new hazard), so a measured patch is exactly the current state.
                if (c.done)
                    c.done->measurement = bridge.renderAndMeasure (c.render);
                break;
        }
    }

    // Fold a governed command into the coordination state on the message thread and broadcast the change (C2
    // parity). Returns how the conflict tier resolved it. Used by the Govern command, and internally to bump the
    // patch generation on a whole-patch change and to release a departing controller's leases.
    Resolution applyGoverned (const GovCmd& g, int by)
    {
        Resolution res;
        const GovState next = governed.apply (g, res);
        if (! (next == governed))
        {
            governed = next;
            broadcastGovernedChanged (by, res);
        }
        return res;
    }

    // -------- JSON reply helpers ----------------------------------------------------------------
    template <typename Fill>
    static juce::String okReply (const juce::var& id, Fill&& fill)
    {
        auto* o = new juce::DynamicObject();
        o->setProperty ("ok", true);
        if (! id.isVoid()) o->setProperty ("id", id);
        fill (*o);
        return juce::JSON::toString (juce::var (o), true);
    }

    static juce::String errorReply (const juce::String& err, const juce::var& id)
    {
        auto* o = new juce::DynamicObject();
        o->setProperty ("ok", false);
        o->setProperty ("error", err);
        if (! id.isVoid()) o->setProperty ("id", id);
        return juce::JSON::toString (juce::var (o), true);
    }

    PluginBridge&  bridge;
    const int      listenPort;

    juce::StreamingSocket listener;

    std::unique_ptr<std::atomic<bool>[]> dirty;          // per-param "changed off the message thread" flag
    int                                  dirtyCount = 0;
    std::atomic<int>                     applyingClientId { 0 };  // set around an apply so a synchronous change event is attributed

    GovState governed;                          // C3 governed coordination state; message-thread-owned (single applier)
    std::vector<std::string> leasableSections;  // the plugin's param groups (bound at construction); the leasable sections

    std::mutex                queueMutex;
    std::deque<Command>       queue;
    static constexpr size_t   kMaxQueue = 4096;

    std::mutex                clientsMutex;
    std::vector<Conn>         clients;
    std::atomic<int>          liveHandlers { 0 };
    std::atomic<int>          nextClientId { 0 };

    std::atomic<Status> status { Status::Idle };

    JUCE_DECLARE_NON_COPYABLE_WITH_LEAK_DETECTOR (ControlServer)
};

} // namespace sidechain
