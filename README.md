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
sampleRate := 1.0

cpu := wzprof.NewCPUProfiler(time.Now)
mem := wzprof.NewMemoryProfiler(time.Now)

ctx := context.WithValue(context.Background(),
	experimental.FunctionListenerFactoryKey{},
	experimental.MultiFunctionListenerFactory(
        wzprof.SampledFunctionListenerFactory(sampleRate, cpu),
        wzprof.SampledFunctionListenerFactory(sampleRate, mem),
    ),
)

runtime := wazero.NewRuntime(ctx)
defer runtime.Close(ctx)

compiledModule, err := runtime.CompileModule(ctx, wasmCode)
if err != nil {
	log.Fatal("compiling wasm module:", err)
}

symbols, err := wzprof.BuildDwarfSymbolizer(compiledModule)
if err != nil {
	log.Fatal("symbolizing wasm module:", err)
}

// The CPU profiler collects records of module execution between two time
// points, the program drives where the profiler is active by calling
// StartProfile/StopProfile.
cpu.StartProfile()

moduleInstance, err := runtime.InstantiateModule(ctx, compiledModule,
	wazero.NewModuleConfig(),
)
if err != nil {
	log.Fatal("instantiating wasm module:", err)
}
if err := moduleInstance.Close(ctx); err != nil {
    log.Fatal("closing wasm module:", err)
}

cpuProfile := cpu.StopProfile(sampleRate, symbols)
memProfile := mem.NewProfile(sampleRate, symbols)

if err := wzprof.WriteProfile("cpu.pprof", cpuProfile); err != nil {
    log.Fatal("writing CPU profile:", err)
}
if err := wzprof.WriteProfile("mem.pprof", memProfile); err != nil {
    log.Fatal("writing memory profile:", err)
}
```

Note that the program must spearate the compilation and instantiation of
WebAssembly modules in order to use the profilers, because the module must be
compiled first in order to build the list of symbols from the DWARF sections.

### Examples

#### Connect to running pprof server

```
wzprof -pprof-addr :8080 path/to/guest.wasm
```
```
go tool pprof -http :3030 http://localhost:8080/debug/pprof/profile?seconds=5
```
```
go tool pprof -http :3030 http://localhost:8080/debug/pprof/heap
```

#### Run program to completion with CPU or memory profiling

```
wzprof -cpuprofile /tmp/profile path/to/guest.wasm
```
```
wzprof -memprofile /tmp/profile path/to/guest.wasm
```
```
go tool pprof -http :4000 /tmp/profile
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
the number of time it sees a unique stack trace.

The CPU time profiler measures the actual time spent on-CPU without taking into
account the off-CPU time (e.g waiting for I/O). For this profiler, all the
host-functions are considered off-CPU.
