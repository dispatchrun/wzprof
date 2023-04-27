[![Build](https://github.com/stealthrocket/wazero-profiler/actions/workflows/go.yml/badge.svg)](https://github.com/stealthrocket/wazero-profiler/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/stealthrocket/wazero-profiler.svg)](https://pkg.go.dev/github.com/stealthrocket/wazero-profiler)
[![Apache 2 License](https://img.shields.io/badge/license-Apache%202-blue.svg)](LICENSE)

# wazero-profiler

`wazero-profiler` provides mechanisms to profile CPU, Memory (and soon more) of
guest running within the Wazero runtime.

## Motivation

`wazero-profiler` brings `pprof` to Wazero. `pprof` is one of the favorite profiling tool of any Go developer.
Many WASM runtimes out there allow to profile guest code via an external profiler, such as `perf` but oftentime 
the user experience is not great and such profilers might be hard to use in a distributed environment.

`wazero-profiler` currently implements two profilers, CPU and Memory, and works with any language compiled to WASM.
Developers can use the classic `go tool pprof` or any `pprof` compatible tool to consume their profiles.

## Usage

You can either use `wazero-profiler` as a CLI or as a library if you use the Wazero runtime libraries.

You can get the latest version of the profiler via:
```
go get github.com/stealthrocket/wazero-profiler@latest
```

Or if you want to use the CLI:
```
go install github.com/stealthrocket/wazero-profiler/cmd/wazero-profiler@latest
```

### Example with go tool

```
wazero-profiler -http=:8080 path/to/guest.wasm
```

```
go tool pprof -http=:3030 http://localhost:8080
```
