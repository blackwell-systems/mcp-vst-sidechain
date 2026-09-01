#pragma once
#include <juce_audio_processors/juce_audio_processors.h>
#include <juce_audio_basics/juce_audio_basics.h>
#include <memory>
#include <string>
#include "ControlServer.h"
#include "JucePluginBridge.h"

// ================================================================================================
// sidechain::Host - a headless child-plugin host. Loads ONE VST3/AU by path via
// juce::AudioPluginFormatManager, instantiates it, prepares it for playback, and points a ControlServer (via a
// JucePluginBridge) at the hosted processor so an agent can drive it over the wire protocol.
//
// This is Sidechain's reason to exist (the generic control substrate - ControlServer over a PluginBridge - plus
// the ability to wrap an arbitrary closed-source plugin the user has licensed). It talks to the plugin only through the
// standardized VST3/AU API JUCE exposes: no source, no reverse-engineering, exactly what a DAW does.
//
// The parameter catalog (enumerateCatalog) is the runtime replacement for a fixed, codegen'd catalog: it
// walks the loaded plugin's automatable parameters and emits the JSON the Go MCP server reads. Catalog quality
// is only as good as the plugin's metadata (well-behaved plugins name params cleanly; many expose "Param 47");
// an LLM naming/semantics layer over that surface is future work.
// ================================================================================================

namespace sidechain
{

class Host
{
public:
    Host();
    ~Host();

    // Load a plugin by filesystem path (a .vst3 bundle or .component AU) OR by an AudioUnit component identifier
    // of the form "AudioUnit:Synths/aumu,dls ,appl" (type,subtype,manufacturer). The identifier form is how an AU
    // is reliably referenced on macOS - the OS resolves it through the AudioComponent registry, not a path.
    // Returns an error string on failure (empty on success). Prepares the plugin at the given rate / block size.
    juce::String load (const juce::String& pluginPath, double sampleRate = 48000.0, int blockSize = 512);

    bool isLoaded() const noexcept { return plugin != nullptr; }

    juce::AudioProcessor* getProcessor() const noexcept { return plugin.get(); }
    juce::MidiKeyboardState& getKeyboardState() noexcept { return keyboardState; }

    // Enumerate the loaded plugin's automatable parameters into the catalog JSON the Go server consumes
    // (fields: id/label/group/type/min/max/step/default/choices). Empty string if no plugin is loaded.
    juce::String enumerateCatalog() const;

    // Start / stop the ControlServer pointed at the hosted processor (through a JucePluginBridge). Off until
    // startControl is called, so loading a plugin opens no socket by itself.
    juce::String startControl (int port = ControlServer::kDefaultPort);
    void stopControl();

    ControlServer::Status controlStatus() const noexcept
    {
        return control != nullptr ? control->getStatus() : ControlServer::Status::Idle;
    }

private:
    juce::AudioPluginFormatManager formatManager;
    juce::MidiKeyboardState        keyboardState;
    std::unique_ptr<juce::AudioPluginInstance> plugin;
    std::unique_ptr<JucePluginBridge>          bridge;    // must outlive control (control holds bridge&)
    std::unique_ptr<ControlServer>             control;

    double preparedRate = 48000.0;
    int    preparedBlock = 512;

    JUCE_DECLARE_NON_COPYABLE_WITH_LEAK_DETECTOR (Host)
};

} // namespace sidechain
