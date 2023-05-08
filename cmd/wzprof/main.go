//  Copyright 2023 Stealth Rocket, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
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
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

const defaultSampleRate = 1.0 / 19

type program struct {
	filePath   string
	pprofAddr  string
	cpuProfile string
	memProfile string
	sampleRate float64
	hostTime   bool
	mounts     []string
}

func (prog *program) run(ctx context.Context) error {
	wasmName := filepath.Base(prog.filePath)
	wasmCode, err := os.ReadFile(prog.filePath)
	if err != nil {
		return fmt.Errorf("loading wasm module: %w", err)
	}

	cpu := wzprof.NewCPUProfiler(wzprof.EnableHostTime(prog.hostTime))
	mem := wzprof.NewMemoryProfiler()

	var listeners []experimental.FunctionListenerFactory
	if prog.cpuProfile != "" || prog.pprofAddr != "" {
		listeners = append(listeners, wzprof.SampledFunctionListenerFactory(prog.sampleRate, cpu))
	}
	if prog.memProfile != "" || prog.pprofAddr != "" {
		listeners = append(listeners, wzprof.SampledFunctionListenerFactory(prog.sampleRate, mem))
	}

	ctx = context.WithValue(ctx,
		experimental.FunctionListenerFactoryKey{},
		experimental.MultiFunctionListenerFactory(listeners...),
	)

	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithDebugInfoEnabled(true).
		WithCustomSections(true))

	compiledModule, err := runtime.CompileModule(ctx, wasmCode)
	if err != nil {
		return fmt.Errorf("compiling wasm module: %w", err)
	}

	symbols, err := wzprof.BuildDwarfSymbolizer(compiledModule)
	if err != nil {
		return fmt.Errorf("symbolizing wasm module: %w", err)
	}

	if prog.pprofAddr != "" {
		pprof := http.NewServeMux()
		pprof.Handle("/debug/pprof/profile", cpu.NewHandler(prog.sampleRate, symbols))
		pprof.Handle("/debug/pprof/heap", mem.NewHandler(prog.sampleRate, symbols))
		pprof.Handle("/wzprof", http.DefaultServeMux)

		go func() {
			if err := http.ListenAndServe(prog.pprofAddr, pprof); err != nil {
				log.Println(err)
			}
		}()
	}

	if prog.cpuProfile != "" {
		cpu.StartProfile()
		defer func() {
			writeProfile(prog.cpuProfile, cpu.StopProfile(prog.sampleRate, symbols))
		}()
	}

	if prog.memProfile != "" {
		defer func() {
			writeProfile(prog.memProfile, mem.NewProfile(prog.sampleRate, symbols))
		}()
	}

	ctx, cancel := context.WithCancelCause(ctx)
	go func() {
		defer cancel(nil)
		wasi_snapshot_preview1.MustInstantiate(ctx, runtime)

		config := wazero.NewModuleConfig().
			WithStdout(os.Stdout).
			WithStderr(os.Stderr).
			WithStdin(os.Stdin).
			WithRandSource(rand.Reader).
			WithSysNanosleep().
			WithSysNanotime().
			WithSysWalltime().
			WithArgs(wasmName).
			WithFSConfig(createFSConfig(prog.mounts))

		instance, err := runtime.InstantiateModule(ctx, compiledModule, config)
		if err != nil {
			cancel(fmt.Errorf("instantiating module: %w", err))
			return
		}
		if err := instance.Close(ctx); err != nil {
			cancel(fmt.Errorf("closing module: %w", err))
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
	pprofAddr  string
	cpuProfile string
	memProfile string
	sampleRate float64
	hostTime   bool
	mounts     string
)

func init() {
	log.Default().SetOutput(os.Stderr)
	flag.StringVar(&pprofAddr, "pprof-addr", "", "Address where to expose a pprof HTTP endpoint.")
	flag.StringVar(&cpuProfile, "cpuprofile", "", "Write a CPU profile to the specified file before exiting.")
	flag.StringVar(&memProfile, "memprofile", "", "Write a memory profile to the specified file before exiting.")
	flag.Float64Var(&sampleRate, "sample-rate", defaultSampleRate, "Set the profile sampling rate (0-1).")
	flag.BoolVar(&hostTime, "host", false, "Include time spent in host function calls.")
	flag.StringVar(&mounts, "mount", "", "Comma-separated list of directories to mount (e.g. /tmp:/tmp:ro).")
}

func run(ctx context.Context) error {
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		// TODO: pring flag usage
		return fmt.Errorf("usage: wzprof </path/to/app.wasm>")
	}

	return (&program{
		filePath:   args[0],
		pprofAddr:  pprofAddr,
		cpuProfile: cpuProfile,
		memProfile: memProfile,
		sampleRate: sampleRate,
		hostTime:   hostTime,
		mounts:     split(mounts),
	}).run(ctx)
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func writeProfile(path string, prof *profile.Profile) {
	if err := wzprof.WriteProfile(path, prof); err != nil {
		log.Fatalf("ERROR: writing profile: %s", err)
	}
}

func createFSConfig(mounts []string) wazero.FSConfig {
	fs := wazero.NewFSConfig()
	for _, m := range mounts {
		parts := strings.Split(m, ":")
		if len(parts) < 2 {
			log.Fatalf("invalid mount: %s", m)
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
