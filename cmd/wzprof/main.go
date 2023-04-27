package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/google/pprof/profile"
	"github.com/stealthrocket/wzprof"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

var (
	file      = flag.String("file", "", "Filename to write profile to")
	httpAddr  = flag.String("http", "", "HTTP server address")
	sampling  = flag.Float64("sampling", wzprof.DefaultCPUSampling, "CPU sampling rate")
	profilers = flag.String("profilers", "cpu,mem", "Comma-separated list of profilers to use")
)

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		return fmt.Errorf("usage: wzprof </path/to/app.wasm>")
	}
	wasmPath := args[0]
	wasmName := filepath.Base(wasmPath)
	wasmCode, err := os.ReadFile(wasmPath)
	if err != nil {
		return fmt.Errorf("cannot open WASM file at '%s': %w", wasmPath, err)
	}

	pfs := []wzprof.Profiler{}
	pfnames := strings.Split(*profilers, ",")
	for _, name := range pfnames {
		switch name {
		case "cpu":
			pfs = append(pfs, &wzprof.ProfilerCPU{
				Sampling: float32(*sampling),
			})
		case "mem":
			pfs = append(pfs, &wzprof.ProfilerMemory{})
		}
	}

	pl := wzprof.NewProfileListener(pfs...)
	ctx = pl.Register(ctx)

	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigCompiler())
	defer runtime.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, runtime)

	config := wazero.NewModuleConfig().
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		WithStdin(os.Stdin).
		WithRandSource(rand.Reader).
		WithSysNanosleep().
		WithSysNanotime().
		WithSysWalltime().
		WithArgs(wasmName)

	go func() {
		instance, err := runtime.InstantiateWithConfig(ctx, wasmCode, config)
		if err != nil {
			fmt.Println(err)
		}

		if err := instance.Close(ctx); err != nil {
			fmt.Println(err)
		}

		cancel()
	}()

	if *httpAddr != "" {
		go func() {
			if err := http.ListenAndServe(*httpAddr, pl); err != nil {
				log.Println(err)
			}
		}()
	}

	<-ctx.Done()
	cancel()

	if *file != "" {
		if err := writeFile(*file, pl.BuildProfile()); err != nil {
			return err
		}
	}

	return nil
}

func writeFile(fname string, p *profile.Profile) error {
	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer f.Close()

	return p.Write(f)
}
