#include "Host.h"

namespace sidechain
{

Host::Host()
{
    // Register the platform plugin formats JUCE was built with (VST3 everywhere; AudioUnit on macOS).
    formatManager.addDefaultFormats();
}

Host::~Host()
{
    stopControl();
    if (plugin != nullptr)
        plugin->releaseResources();
}

juce::String Host::load (const juce::String& pluginPath, double sampleRate, int blockSize)
{
    stopControl();
    plugin.reset();

    juce::File file (pluginPath);
    if (! file.exists())
        return "plugin not found: " + pluginPath;

    // Ask each registered format to describe the binary. A single plugin file may host several sub-plugins
    // (shell format); we take the first described type.
    juce::OwnedArray<juce::PluginDescription> found;
    juce::KnownPluginList list;
    for (auto* fmt : formatManager.getFormats())
        list.scanAndAddFile (pluginPath, /*dontRescanIfAlreadyInList*/ false, found, *fmt);

    if (found.isEmpty())
        return "no VST3/AU plugin described by: " + pluginPath;

    juce::String err;
    plugin = formatManager.createPluginInstance (*found.getFirst(), sampleRate, blockSize, err);
    if (plugin == nullptr)
        return "createPluginInstance failed: " + err;

    preparedRate = sampleRate;
    preparedBlock = blockSize;
    plugin->enableAllBuses();
    plugin->prepareToPlay (sampleRate, blockSize);
    return {};
}

juce::String Host::enumerateCatalog() const
{
    if (plugin == nullptr)
        return {};

    auto* root = new juce::DynamicObject();
    root->setProperty ("stateRootTag", "PARAMS");
    root->setProperty ("stateVersion", 0);   // opaque to the bridge; a host may stamp its own

    // Walk on the BASE AudioProcessorParameter API, not RangedAudioParameter. A HOSTED VST3/AU exposes its
    // parameters as juce::HostedAudioProcessorParameter (an AudioProcessorParameter that is NOT a
    // RangedAudioParameter), so a RangedAudioParameter cast would drop every hosted param. The base API gives
    // everything the bridge needs: a normalized value/default, a step count, and value<->text via the
    // plugin's own formatter. Real-unit endpoints come from getText(0/1). A stable string id comes from a
    // HostedAudioProcessorParameter when available, else the parameter index.
    juce::Array<juce::var> params;
    const auto& all = plugin->getParameters();
    for (auto* p : all)
    {
        if (! p->isAutomatable())
            continue;

        juce::String id = juce::String (p->getParameterIndex());
        if (auto* hp = dynamic_cast<juce::HostedAudioProcessorParameter*> (p))
            if (hp->getParameterID().isNotEmpty())
                id = hp->getParameterID();

        auto* one = new juce::DynamicObject();
        one->setProperty ("id",    id);
        one->setProperty ("label", p->getName (256));

        // Type + choices from the base API. A parameter with discrete steps and value-strings is a choice; a
        // 2-step boolean is a bool; a stepped numeric is an int; otherwise a continuous float.
        const int steps = p->getNumSteps();
        const auto valueStrings = p->getAllValueStrings();

        juce::String type = "float";
        juce::Array<juce::var> choices;
        if (p->isDiscrete() && ! valueStrings.isEmpty())
        {
            type = "choice";
            for (const auto& c : valueStrings)
                choices.add (c);
        }
        else if (p->isBoolean() || steps == 2)
        {
            type = "bool";
        }
        else if (p->isDiscrete())
        {
            type = "int";
        }

        // Endpoints. For a plain 0..1 float we report 0..1; for a discrete param we report the index range
        // (0..steps-1). The Go side's normalize math is linear over [min,max], matching this.
        double lo = 0.0, hi = 1.0, step = 0.0, def = p->getDefaultValue();
        if (type == "choice" || type == "int" || type == "bool")
        {
            const double n = (steps > 1) ? (double) (steps - 1) : 1.0;
            lo = 0.0; hi = n; step = 1.0;
            def = juce::jlimit (0.0, n, p->getDefaultValue() * n);
        }

        one->setProperty ("group",   juce::String());   // best-effort; most formats expose no category here
        one->setProperty ("type",    type);
        one->setProperty ("min",     lo);
        one->setProperty ("max",     hi);
        one->setProperty ("step",    step);
        one->setProperty ("default", def);
        if (! choices.isEmpty())
            one->setProperty ("choices", choices);

        params.add (juce::var (one));
    }

    root->setProperty ("count",  params.size());
    root->setProperty ("params", params);
    return juce::JSON::toString (juce::var (root), false);
}

juce::String Host::startControl (int port)
{
    if (plugin == nullptr)
        return "no plugin loaded";
    if (control != nullptr)
        return {};   // already listening
    control = std::make_unique<ControlListener> (*plugin, keyboardState, port);
    return {};
}

void Host::stopControl()
{
    control.reset();
}

} // namespace sidechain
