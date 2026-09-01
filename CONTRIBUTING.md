# Contributing to Sidechain

`mcp-vst-sidechain` ("Sidechain") is a generic MCP bridge: a JUCE host that wraps any VST3/AU plugin and
exposes its parameters to an AI agent in realtime, with a Go MCP server that speaks the control protocol and
GCF-encodes large payloads. Two layers, two languages (that split is deliberate; see
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)).

Contributions are welcome. Please keep changes focused and covered.

## Repository layout

```
.                     Go module (github.com/blackwell-systems/mcp-vst-sidechain)
  catalog.go          parameter catalog + pure param math (normalize/clamp/choice)
  live.go             the live-control client (LiveEndpoint) over the C++ listener socket
  gcf.go              GCF-encode the model-facing tool output (token-compact)
  paramtools.go       list/get/set_param(s) behind RegisterParamTools
  server.go           the headless stdio MCP server
  cmd/sidechain/      the server binary
cpp/
  ControlListener.h   the in-process control listener for any juce::AudioProcessor
  Host.h / Host.cpp   the child-plugin host (AudioPluginFormatManager)
  main.cpp            the headless host CLI
  CMakeLists.txt      JUCE via FetchContent
```

## Building and testing

### Go layer

```bash
go build ./...
go vet ./...
go test ./...
```

The tests cover the catalog math, the batch `set_params` (JSON and GCF input), and a loopback test that stands
up a fake host speaking the wire protocol and drives it end to end. No plugin or socket is required.

### C++ host

```bash
cmake -S cpp -B cpp/build -DCMAKE_BUILD_TYPE=Release
cmake --build cpp/build --target sidechain-host
```

The first configure fetches JUCE via FetchContent, so it is network- and time-heavy. VST3 hosting works on all
platforms; AU hosting is macOS only.

## Guidelines

- **The wire protocol is a contract.** The Go client and the C++ listener speak the identical line-delimited
  JSON protocol. Change one side and you change both; the loopback test asserts "same bytes on the socket."
- **Keep the control path off the audio thread.** All parameter/state mutations go through the message-thread
  drain (`setValueNotifyingHost`), never the audio callback. Reads snapshot atomics.
- **Loopback binding only.** The listener binds `127.0.0.1` exclusively. Do not add a routable bind.
- **Prose style:** no em dashes in prose, docs, or comments (use colons, commas, semicolons, or parentheses).
- Add a test with any behavior change and keep `go vet` clean.

## Reporting bugs

Open an issue at [github.com/blackwell-systems/mcp-vst-sidechain](https://github.com/blackwell-systems/mcp-vst-sidechain/issues)
with the plugin (format + name), platform, and steps to reproduce.

## License

By contributing you agree your contributions are licensed under the [MIT License](LICENSE).
