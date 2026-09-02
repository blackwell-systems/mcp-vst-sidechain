#include "Host.h"
#include "Sectioning.h"

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

    // An AudioUnit is normally referenced not by a filesystem path but by a component IDENTIFIER of the form
    // "AudioUnit:Synths/aumu,dls ,appl" (type,subtype,manufacturer). This is how JUCE's KnownPluginList records
    // and reloads AUs, and it is the reliable handle on macOS: the OS resolves an AU through the AudioComponent
    // registry (AudioComponentFindNext), not by scanning an arbitrary path. A raw ".component" path only loads
    // when that component is ALSO registered with the system (installed under ~/Library/Audio/Plug-Ins/Components
    // or /Library/... and picked up by AudioComponentRegistrar); JUCE parses the path's Info.plist but still does
    // the registry lookup. So we accept either form: an identifier bypasses the file check (there is no file to
    // stat), a path is validated as before.
    const bool isAUIdentifier = pluginPath.startsWithIgnoreCase ("AudioUnit:");
    if (! isAUIdentifier)
    {
        juce::File file (pluginPath);
        if (! file.exists())
            return "plugin not found: " + pluginPath;
    }

    // Ask each registered format to describe the binary. A single plugin file may host several sub-plugins
    // (shell format); we take the first described type.
    juce::OwnedArray<juce::PluginDescription> found;
    juce::KnownPluginList list;
    for (auto* fmt : formatManager.getFormats())
        list.scanAndAddFile (pluginPath, /*dontRescanIfAlreadyInList*/ false, found, *fmt);

    if (found.isEmpty())
        return "no VST3/AU plugin described by: " + pluginPath;

    juce::String err;
    loadedDesc = *found.getFirst();   // retain the plugin identity for the catalog fingerprint
    plugin = formatManager.createPluginInstance (loadedDesc, sampleRate, blockSize, err);
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

    // Plugin identity, for the Go side's semantic-store fingerprint (name|manufacturer|format|paramIDs|count).
    // The version is recorded but not part of the fingerprint (a surface-preserving bump reuses the cache).
    auto* ident = new juce::DynamicObject();
    ident->setProperty ("name",         loadedDesc.name);
    ident->setProperty ("manufacturer", loadedDesc.manufacturerName);
    ident->setProperty ("format",       loadedDesc.pluginFormatName);
    ident->setProperty ("version",      loadedDesc.version);
    ident->setProperty ("uniqueId",     loadedDesc.uniqueId);
    root->setProperty ("plugin", juce::var (ident));

    // Walk on the BASE AudioProcessorParameter API, not RangedAudioParameter. A HOSTED VST3/AU exposes its
    // parameters as juce::HostedAudioProcessorParameter (an AudioProcessorParameter that is NOT a
    // RangedAudioParameter), so a RangedAudioParameter cast would drop every hosted param. The base API gives
    // everything the bridge needs: a normalized value/default, a step count, and value<->text via the
    // plugin's own formatter. Real-unit endpoints come from getText(0/1). A stable string id comes from a
    // HostedAudioProcessorParameter when available, else the parameter index.
    // Section each param. `group` is the raw parameter-tree group (VST3 unit / AU clump), empty for a flat plugin.
    // `section` is the EFFECTIVE navigable section: that group when present, else one derived from shared label
    // prefixes, else "other". computeSections (Sectioning.h) is the single source of truth - the same computation
    // the C3 governed layer uses for leasable sections - so the host, not the Go side, derives sections.
    const auto sections = computeSections (*plugin);

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

        // A native RangedAudioParameter (our own plugin) owns a real<->normalized curve; a hosted VST3/AU param
        // does not (base API only). Only continuous native params get REAL endpoints; discrete/choice/bool stay
        // on the index representation below either way (skew is a continuous-value concern).
        auto* ranged = dynamic_cast<juce::RangedAudioParameter*> (p);

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

        // Endpoints. A continuous NATIVE param reports its REAL range/default through the plugin's own
        // NormalisableRange (skew lives below the 0..1, so the Go side forwards real values and the plugin
        // denormalises). A hosted continuous param reports 0..1 (no real scalar). A discrete param reports the
        // index range (0..steps-1); the Go side's linear normalize matches an integer step range.
        const bool useReal = (ranged != nullptr && type == "float");
        double lo = 0.0, hi = 1.0, step = 0.0, def = p->getDefaultValue();
        if (useReal)
        {
            const auto& nr = ranged->getNormalisableRange();
            lo = nr.start; hi = nr.end; step = nr.interval;
            def = ranged->convertFrom0to1 (p->getDefaultValue());
        }
        else if (type == "choice" || type == "int" || type == "bool")
        {
            const double n = (steps > 1) ? (double) (steps - 1) : 1.0;
            lo = 0.0; hi = n; step = 1.0;
            def = juce::jlimit (0.0, n, p->getDefaultValue() * n);
        }

        const auto rg = sections.rawGroup.find (p);
        const auto ef = sections.effective.find (p);
        one->setProperty ("group",        rg != sections.rawGroup.end() ? juce::String (rg->second) : juce::String());
        one->setProperty ("section",      ef != sections.effective.end() ? juce::String (ef->second) : juce::String ("other"));
        one->setProperty ("type",         type);
        one->setProperty ("min",          lo);
        one->setProperty ("max",          hi);
        one->setProperty ("step",         step);
        one->setProperty ("default",      def);
        one->setProperty ("hasRealRange", useReal);
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
    // The bridge is the plugin-specific half (parameter/state/MIDI access); the server is the VST-agnostic
    // control plane driving it. The bridge must outlive the server (the server holds a reference to it), so it
    // is constructed first and torn down last.
    bridge  = std::make_unique<JucePluginBridge> (*plugin, keyboardState);
    control = std::make_unique<ControlServer> (*bridge, port);
    return {};
}

void Host::stopControl()
{
    control.reset();   // stop the control plane (and its threads) before releasing the bridge it references
    bridge.reset();
}

} // namespace sidechain
