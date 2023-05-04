[![Build](https://github.com/stealthrocket/wzprof/actions/workflows/go.yml/badge.svg)](https://github.com/stealthrocket/wzprof/actions/workflows/go.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/stealthrocket/wzprof.svg)](https://pkg.go.dev/github.com/stealthrocket/wzprof)
[![Apache 2 License](https://img.shields.io/badge/license-Apache%202-blue.svg)](LICENSE)

# wzprof

`wzprof`, pronounced as you think it should, is as pprof based profiler for
WebAssembly built on top of Wazero. It offers the ability to collect CPU and
Memory profiles during the execution of WebAssembly modules.

## Motivation

WebAssembly runtimes typically allow profiling guest code via an external
profiler such as `perf`, but in many cases the recording and analysis of
profiles remains a difficult task, especially due to features like JIT
compilation.

`pprof` is the de-factor standard profiling tool for Go programs, and offers
some of the simplest and quickest ways to gather insight into the performance
of an application.

`wzprof` aims the combine the capabilities and user experience of `pprof`
within a Wazero runtime, enabling the profiling of any application compiled
to WebAssembly.

## Features

`wzprof` mimics the approach and workflow popularized by Go pprof, and extends
it to collect profiles of WebAssembly programs compiled from any programming
language. The profiles produced are designed to be compatible with pprof,
allowing developers to use the classic `go tool pprof` workflow to analyize
application performance.

- CPU: calls sampling and on-CPU time.
- Memory: allocations (see below).
- DWARF support (demangling, source-level profiling)
- Integrated pprof server.
- Library and CLI interfaces.

## Usage

You can either use `wzprof` as a CLI or as a library if you use the Wazero
runtime libraries.

You can install the latest version of `wzprof` via:
```
go install github.com/stealthrocket/wzprof/cmd/wzprof@latest
```

To use the library:
```
go get github.com/stealthrocket/wzprof@latest
```

The profiler is propagated to the Wazero runtime through its context:

```
ctx := context.Background()

profiler := wzprof.NewProfileListener(
    wzprof.NewProfilerMemory(),
    wzprof.NewProfilerCPU(0.2),
    wzprof.NewProfilerCPUTime(0.2),
)

ctx = profiler.Register(ctx)

runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
```

### Examples


#### Connect to running pprof server

```
wzprof --pprof-addr=:8080 path/to/guest.wasm
```

```
go tool pprof -http=:3030 http://localhost:8080/guest/debug/pprof
```

#### Run program to completion with profiling

```
wzprof --pprof-file=profile.pb.gz path/to/guest.wasm
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

### CPU

`wzprof` has two CPU profilers: CPU samples and CPU time.

The CPU samples profiler gives a repesentation of the guest execution by counting
the number of time it seen an unique stack trace.

The CPU time profiler measures the actual time spent on-CPU without taking into
account the off-CPU time (e.g waiting for I/O). For this profiler, all the
host-functions are considered off-CPU.
