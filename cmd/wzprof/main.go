package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"

	"github.com/google/pprof/profile"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"

	"github.com/stealthrocket/wzprof"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := run(ctx); err != nil {
		stderr.Print(err)
		os.Exit(1)
	}
}

const defaultSampleRate = 1.0 / 19

type program struct {
	filePath    string
	args        []string
	pprofAddr   string
	cpuProfile  string
	memProfile  string
	sampleRate  float64
	hostProfile bool
	hostTime    bool
	inuseMemory bool
	mounts      []string
}

func (prog *program) run(ctx context.Context) error {
	wasmName := filepath.Base(prog.filePath)
	wasmCode, err := os.ReadFile(prog.filePath)
	if err != nil {
		return fmt.Errorf("loading wasm module: %w", err)
	}

	cpu := wzprof.NewCPUProfiler(wzprof.HostTime(prog.hostTime))
	mem := wzprof.NewMemoryProfiler(wzprof.InuseMemory(prog.inuseMemory))

	var listeners []experimental.FunctionListenerFactory
	if prog.cpuProfile != "" || prog.pprofAddr != "" {
		stdout.Printf("enabling cpu profiler")
		listeners = append(listeners, cpu)
	}
	if prog.memProfile != "" || prog.pprofAddr != "" {
		stdout.Printf("enabling memory profiler")
		listeners = append(listeners, mem)
	}
	if prog.sampleRate < 1 {
		stdout.Printf("configuring sampling rate to %.2g%%", prog.sampleRate)
		for i, lstn := range listeners {
			listeners[i] = wzprof.Sample(prog.sampleRate, lstn)
		}
	}

	ctx = context.WithValue(ctx,
		experimental.FunctionListenerFactoryKey{},
		experimental.MultiFunctionListenerFactory(listeners...),
	)

	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithDebugInfoEnabled(true).
		WithCustomSections(true))

	stdout.Printf("compiling wasm module %s", prog.filePath)
	compiledModule, err := runtime.CompileModule(ctx, wasmCode)
	if err != nil {
		return fmt.Errorf("compiling wasm module: %w", err)
	}

	stdout.Printf("building dwarf symbolizer from compiled wasm module")
	symbols, err := wzprof.BuildDwarfSymbolizer(compiledModule)
	if err != nil {
		symbols, err = wzprof.BuildPclntabSymbolizer(wasmCode)
	}
	if err != nil {
		return fmt.Errorf("symbolizing wasm module: %w", err)
	}

	if prog.pprofAddr != "" {
		u := &url.URL{Scheme: "http", Host: prog.pprofAddr, Path: "/debug/pprof"}
		stdout.Printf("starting prrof http sever at %s", u)

		server := http.NewServeMux()
		server.Handle("/debug/pprof/", wzprof.Handler(prog.sampleRate, symbols, cpu, mem))

		go func() {
			if err := http.ListenAndServe(prog.pprofAddr, server); err != nil {
				stderr.Println(err)
			}
		}()
	}

	if prog.hostProfile {
		if prog.cpuProfile != "" {
			f, err := os.Create(prog.cpuProfile)
			if err != nil {
				return err
			}
			startCPUProfile(f)
			defer stopCPUProfile(f)
		}

		if prog.memProfile != "" {
			f, err := os.Create(prog.memProfile)
			if err != nil {
				return err
			}
			defer writeHeapProfile(f)
		}
	}

	if prog.cpuProfile != "" {
		cpu.StartProfile()
		defer func() {
			p := cpu.StopProfile(prog.sampleRate, symbols)
			if !prog.hostProfile {
				writeProfile("cpu", wasmName, prog.cpuProfile, p)
			}
		}()
	}

	if prog.memProfile != "" {
		defer func() {
			p := mem.NewProfile(prog.sampleRate, symbols)
			if !prog.hostProfile {
				writeProfile("memory", wasmName, prog.memProfile, p)
			}
		}()
	}

	ctx, cancel := context.WithCancelCause(ctx)
	go func() {
		defer cancel(nil)
		stdout.Printf("instantiating host module: wasi_snapshot_preview1")
		wasi_snapshot_preview1.MustInstantiate(ctx, runtime)

		config := wazero.NewModuleConfig().
			WithStdout(os.Stdout).
			WithStderr(os.Stderr).
			WithStdin(os.Stdin).
			WithRandSource(rand.Reader).
			WithSysNanosleep().
			WithSysNanotime().
			WithSysWalltime().
			WithArgs(append([]string{wasmName}, prog.args...)...).
			WithFSConfig(createFSConfig(prog.mounts))

		moduleName := compiledModule.Name()
		if moduleName == "" {
			moduleName = wasmName
		}
		stdout.Printf("instantiating guest module: %s", moduleName)
		instance, err := runtime.InstantiateModule(ctx, compiledModule, config)
		if err != nil {
			cancel(fmt.Errorf("instantiating guest module: %w", err))
			return
		}
		if err := instance.Close(ctx); err != nil {
			cancel(fmt.Errorf("closing guest module: %w", err))
			return
		}
	}()

	<-ctx.Done()
	return silenceContextCanceled(context.Cause(ctx))
}

func silenceContextCanceled(err error) error {
	if err == context.Canceled {
		err = nil
	}
	return err
}

var (
	pprofAddr    string
	cpuProfile   string
	memProfile   string
	sampleRate   float64
	hostProfile  bool
	hostTime     bool
	inuseMemory  bool
	verbose      bool
	mounts       string
	printVersion bool

	version = "dev"
	stdout  = log.Default()
	stderr  = log.New(os.Stderr, "ERROR: ", 0)
)

func init() {
	flag.StringVar(&pprofAddr, "pprof-addr", "", "Address where to expose a pprof HTTP endpoint.")
	flag.StringVar(&cpuProfile, "cpuprofile", "", "Write a CPU profile to the specified file before exiting.")
	flag.StringVar(&memProfile, "memprofile", "", "Write a memory profile to the specified file before exiting.")
	flag.Float64Var(&sampleRate, "sample", defaultSampleRate, "Set the profile sampling rate (0-1).")
	flag.BoolVar(&hostProfile, "host", false, "Generate profiles of the host instead of the guest application.")
	flag.BoolVar(&hostTime, "iowait", false, "Include time spent waiting on I/O in guest CPU profile.")
	flag.BoolVar(&inuseMemory, "inuse", false, "Include snapshots of memory in use (experimental).")
	flag.BoolVar(&verbose, "verbose", false, "Enable more output")
	flag.StringVar(&mounts, "mount", "", "Comma-separated list of directories to mount (e.g. /tmp:/tmp:ro).")
	flag.BoolVar(&printVersion, "version", false, "Print the wzprof version.")
}

func run(ctx context.Context) error {
	flag.Parse()

	if printVersion {
		fmt.Printf("wzprof version %s\n", version)
		return nil
	}

	args := flag.Args()
	if len(args) < 1 {
		// TODO: print flag usage
		return fmt.Errorf("usage: wzprof </path/to/app.wasm>")
	}

	if verbose {
		log.SetPrefix("==> ")
		log.SetFlags(0)
		log.SetOutput(os.Stdout)
	} else {
		log.SetOutput(io.Discard)
	}

	filePath := args[0]

	rate := int(math.Ceil(1 / sampleRate))
	runtime.SetBlockProfileRate(rate)
	runtime.SetMutexProfileFraction(rate)

	return (&program{
		filePath:    filePath,
		args:        args[1:],
		pprofAddr:   pprofAddr,
		cpuProfile:  cpuProfile,
		memProfile:  memProfile,
		sampleRate:  sampleRate,
		hostProfile: hostProfile,
		hostTime:    hostTime,
		inuseMemory: inuseMemory,
		mounts:      split(mounts),
	}).run(ctx)
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func startCPUProfile(f *os.File) {
	if err := pprof.StartCPUProfile(f); err != nil {
		stderr.Print("starting CPU profile:", err)
	}
}

func stopCPUProfile(f *os.File) {
	stdout.Printf("writing host cpu profile to %s", f.Name())
	pprof.StopCPUProfile()
}

func writeHeapProfile(f *os.File) {
	stdout.Printf("writing host memory profile to %s", f.Name())
	if err := pprof.WriteHeapProfile(f); err != nil {
		stderr.Print("writing memory profile:", err)
	}
}

func writeProfile(profileName, wasmName, path string, prof *profile.Profile) {
	m := &profile.Mapping{ID: 1, File: wasmName}
	prof.Mapping = []*profile.Mapping{m}
	stdout.Printf("writing guest %s profile to %s", profileName, path)
	if err := wzprof.WriteProfile(path, prof); err != nil {
		stderr.Print("writing profile:", err)
	}
}

func createFSConfig(mounts []string) wazero.FSConfig {
	fs := wazero.NewFSConfig()
	for _, m := range mounts {
		parts := strings.Split(m, ":")
		if len(parts) < 2 {
			stderr.Fatalf("invalid mount: %s", m)
		}

		var mode string
		if len(parts) == 3 {
			mode = parts[2]
		}

		if mode == "ro" {
			fs = fs.WithReadOnlyDirMount(parts[0], parts[1])
			continue
		}

		fs = fs.WithDirMount(parts[0], parts[1])
	}
	return fs
}
