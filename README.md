[![Build](https://github.com/stealthrocket/wzprof/actions/workflows/build.yml/badge.svg)](https://github.com/stealthrocket/wzprof/actions/workflows/build.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/stealthrocket/wzprof)](https://goreportcard.com/report/github.com/stealthrocket/wzprof)
[![Go Reference](https://pkg.go.dev/badge/github.com/stealthrocket/wzprof.svg)](https://pkg.go.dev/github.com/stealthrocket/wzprof)
[![Apache 2 License](https://img.shields.io/badge/license-Apache%202-blue.svg)](LICENSE)

# wzprof

`wzprof`, pronounced as you think it should, is a pprof based profiler for
WebAssembly built on top of [**Wazero**](https://github.com/tetratelabs/wazero).
It offers the ability to collect CPU and Memory profiles during the execution of
WebAssembly modules.

If you are interested in taking a deep-dive into how `wzprof` is built,
you might enjoy reading:

👉 [**Performance in the spotlight: WebAssembly profiling for everyone**](https://blog.stealthrocket.tech/performance-in-the-spotlight-webassembly-profiling-for-everyone)

## Motivation

WebAssembly runtimes typically allow profiling guest code via an external
profiler such as `perf`, but in many cases the recording and analysis of
profiles remains a difficult task, especially due to features like JIT
compilation.

`pprof` is the de-facto standard profiling tool for Go programs, and offers
some of the simplest and quickest ways to gather insight into the performance
of an application.

`wzprof` aims to combine the capabilities and user experience of `pprof`
with a [**wazero.Runtime**](https://pkg.go.dev/github.com/tetratelabs/wazero#Runtime),
enabling the profiling of any application compiled to WebAssembly.

## Features

`wzprof` mimics the approach and workflow popularized by Go pprof, and extends
it to collect profiles of WebAssembly programs compiled from any programming
language. The profiles produced are designed to be compatible with pprof,
allowing developers to use the classic `go tool pprof` workflow to analyize
application performance.

- CPU: calls sampling and on-CPU time.
- Memory: allocations (see below).
- DWARF support (demangling, source-level profiling).
- Integrated pprof server.
- Library and CLI interfaces.

## Usage

You can either use `wzprof` as a CLI or as a library if you use the Wazero
runtime libraries.

To install the latest version of `wzprof`:
```sh
go install github.com/stealthrocket/wzprof/cmd/wzprof@latest
```
To use the library as code in a Go program:
```sh
go get github.com/stealthrocket/wzprof@latest
```

### Sampling 

By default, wzprof will sample calls with a ratio of 1/19. Sampling is used to
limit the overhead of the profilers but the default rate might not be suitable 
in some cases. 
For example, if your processes are short running and you don't see anything in the 
profile, you might want to disable the sampling. To do so, use `-sample 1`.

### Run program to completion with CPU or memory profiling

In those examples we set the sample rate to 1 to capture all samples because the
test programs complete quickly.

```sh
wzprof -sample 1 -memprofile /tmp/profile ./testdata/c/simple.wasm
```
```sh
wzprof -sample 1 -cpuprofile /tmp/profile ./testdata/c/crunch_numbers.wasm
```
```sh
go tool pprof -http :4000 /tmp/profile
```

### Connect to running pprof server

Similarly to [`net/http/pprof`](https://pkg.go.dev/net/http/pprof), `wzprof`
can expose a pprof-compatible http endpoint on behalf of the guest application:

```sh
wzprof -pprof-addr :8080 ...
```
```sh
go tool pprof -http :3030 'http://localhost:8080/debug/pprof/profile?seconds=5'
```
```sh
go tool pprof -http :3030 'http://localhost:8080/debug/pprof/heap'
```

## Profilers

⚠️  The `wzprof` Go APIs depend on Wazero's `experimental` package which makes no
guarantees of backward compatilbity!

The following code snippet demonstrates how to integrate the profilers to a
Wazero runtime within a Go program:

```go
sampleRate := 1.0

p := wzprof.ProfilingFor(wasmCode)
cpu := p.CPUProfiler()
mem := p.MemoryProfiler()

ctx := context.WithValue(context.Background(),
	experimental.FunctionListenerFactoryKey{},
	experimental.MultiFunctionListenerFactory(
		wzprof.Sample(sampleRate, cpu),
		wzprof.Sample(sampleRate, mem),
    ),
)

runtime := wazero.NewRuntime(ctx)
defer runtime.Close(ctx)

compiledModule, err := runtime.CompileModule(ctx, wasmCode)
if err != nil {
	log.Fatal("compiling wasm module:", err)
}

err = p.Prepare(compiledModule)
if err != nil {
	return fmt.Errorf("preparing wasm module: %w", err)
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

### Memory

Memory profiling works by tracing specific functions. Supported functions are:

- `malloc`
- `calloc`
- `realloc`
- `free`
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

## Language support

wzprof runs some heuristics to assess what the guest module is running to adapt
the way it symbolizes and walks the stack. In all other cases, it defaults to
inspecting the wasm stack and uses DWARF information if present in the module.

### Golang

If the guest has been compiled by golang/go 1.21+, wzprof inspects the memory
to walk the Go stack, which provides full call stacks, instead of the shortened
versions you would get without this support.

In addition, wzprof parses pclntab to perform symbolization. This is the same
mechanism the Go runtime itself uses to display meaningful stack traces when a
panic occurs.

### Python 3.11

If the guest is CPython 3.11 and has been compiled with debug symbols (such as
[timecraft's][timecraft-python]), wzprof walks the Python interpreter call
stack, not the C stack it would otherwise report. This provides more meaningful
profiling information on the script being executed.

At the moment it does not support merging the C extension calls into the Python
interpreter stack.

Note that a current limitation of the implementation is that unloading or
reloading modules may result in an incorrect profile. If that's a problem for
you please file an issue in the github tracker.

[timecraft-python]: https://docs.timecraft.dev/getting-started/prep-application/compiling-python#preparing-python

## Contributing

Pull requests are welcome! Anything that is not a simple fix would probably
benefit from being discussed in an issue first.

Remember to be respectful and open minded!
