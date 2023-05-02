[![Build](https://github.com/stealthrocket/wzprof/actions/workflows/go.yml/badge.svg)](https://github.com/stealthrocket/wzprof/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/stealthrocket/wzprof.svg)](https://pkg.go.dev/github.com/stealthrocket/wzprof)
[![Apache 2 License](https://img.shields.io/badge/license-Apache%202-blue.svg)](LICENSE)

# wzprof

`wzprof`, pronounce "wa-zi-prof", is as pprof based profiler for WebAssembly built on top of Wazero.
It gives you easy access to CPU and Memory profiles of your WASM modules.

## Motivation

`pprof` is one of the favorite profiling tool of any Go developer. Many WASM runtimes out there allow 
to profile guest code via an external profiler, such as `perf`, but in many cases, you don't want to run 
an external profiler and you just want a quick and easy access to your WASM module profiles.

`wzprof` currently implements two profilers, CPU and Memory, and works with any language compiled to WASM.
Developers can use the classic `go tool pprof` or any `pprof` compatible tool to consume their profiles.


## Features

- CPU: calls sampling.
- Memory: allocations (see below).
- DWARF support (demangling, source-level profiling)
- Integrated pprof server.
- Library and CLI interfaces.

## Usage

You can either use `wzprof` as a CLI or as a library if you use the Wazero runtime libraries.

You can get the latest version of the profiler library via:
```
go get github.com/stealthrocket/wzprof@latest
```

Or if you want to use the CLI:
```
go install github.com/stealthrocket/wzprof/cmd/wzprof@latest
```

### Examples


#### Connect to running pprof server

```
wzprof -http=:8080 path/to/guest.wasm
```

```
go tool pprof -http=:3030 http://localhost:8080
```

#### Run program to completion with profiling

```
wzprof -file=profile.pb.gz path/to/guest.wasm
```

## Profilers

### Memory

Memory profiling works by tracing specific functions. Supported functions are:

- `malloc`
- `calloc`
- `realloc`
- `runtime.mallocgc`
- `runtime.alloc`

Feel free to open a pull request to support more memory-allocating functions!