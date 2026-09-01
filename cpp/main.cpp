// main.cpp - the headless Sidechain host CLI. It loads a VST3/AU plugin by path, writes its parameter catalog
// as JSON (for the Go MCP server to read), starts the ControlListener on a loopback port, and then pumps the
// JUCE message loop so the socket serves and parameter/state applies happen on the message thread.
//
//   sidechain-host --plugin /path/to/Plugin.vst3 [--catalog out.json] [--port 51703]
//
// Then run the Go server against the emitted catalog and connect_live to this port:
//   sidechain --catalog out.json         (stdio MCP; the agent's connect_live dials --port here)
//
// This is the C++ half of Option A (two-process, Go split): audio never crosses the process boundary; only
// control/state messages do.

#include <juce_events/juce_events.h>
#include "Host.h"

namespace
{
juce::String argValue (const juce::StringArray& args, const juce::String& flag, const juce::String& def = {})
{
    const int i = args.indexOf (flag);
    if (i >= 0 && i + 1 < args.size())
        return args[i + 1];
    return def;
}
}

// A minimal JUCE app so there IS a message loop for the ControlListener's AsyncUpdater to drain on.
class SidechainHostApp : public juce::JUCEApplicationBase
{
public:
    const juce::String getApplicationName() override    { return "sidechain-host"; }
    const juce::String getApplicationVersion() override { return "0.1.0"; }
    bool moreThanOneInstanceAllowed() override          { return true; }
    void anotherInstanceStarted (const juce::String&) override {}
    void suspended() override {}
    void resumed() override {}
    void unhandledException (const std::exception*, const juce::String&, int) override {}

    void initialise (const juce::String& commandLineParameters) override
    {
        juce::StringArray args;
        args.addTokens (commandLineParameters, true);
        args.trim();

        const juce::String pluginPath = argValue (args, "--plugin");
        const juce::String catalogOut = argValue (args, "--catalog", "plugin-catalog.json");
        const int          port       = argValue (args, "--port", juce::String (sidechain::ControlListener::kDefaultPort)).getIntValue();

        if (pluginPath.isEmpty())
        {
            std::fprintf (stderr, "usage: sidechain-host --plugin <path.vst3|.component> [--catalog out.json] [--port %d]\n",
                          sidechain::ControlListener::kDefaultPort);
            setApplicationReturnValue (2);
            quit();
            return;
        }

        host = std::make_unique<sidechain::Host>();
        if (const auto err = host->load (pluginPath); err.isNotEmpty())
        {
            std::fprintf (stderr, "sidechain-host: load failed: %s\n", err.toRawUTF8());
            setApplicationReturnValue (1);
            quit();
            return;
        }

        // Emit the catalog JSON for the Go server.
        const juce::String catalog = host->enumerateCatalog();
        if (! juce::File (catalogOut).replaceWithText (catalog))
            std::fprintf (stderr, "sidechain-host: warning: could not write catalog to %s\n", catalogOut.toRawUTF8());
        else
            std::fprintf (stderr, "sidechain-host: wrote catalog to %s\n", catalogOut.toRawUTF8());

        if (const auto err = host->startControl (port); err.isNotEmpty())
        {
            std::fprintf (stderr, "sidechain-host: startControl failed: %s\n", err.toRawUTF8());
            setApplicationReturnValue (1);
            quit();
            return;
        }
        std::fprintf (stderr, "sidechain-host: listening on 127.0.0.1:%d - run the Go server and connect_live.\n", port);
    }

    void shutdown() override { host.reset(); }
    void systemRequestedQuit() override { quit(); }

private:
    std::unique_ptr<sidechain::Host> host;
};

START_JUCE_APPLICATION (SidechainHostApp)
