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

const defaultCPUSampling = 1

type program struct {
	WasmPath  string
	File      string
	HttpAddr  string
	Sampling  float64
	Profilers string
}

func (prog program) Execute(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wasmName := filepath.Base(prog.WasmPath)
	wasmCode, err := os.ReadFile(prog.WasmPath)
	if err != nil {
		return fmt.Errorf("cannot open WASM file at '%s': %w", prog.WasmPath, err)
	}

	pfs := []wzprof.Profiler{}
	pfnames := strings.Split(prog.Profilers, ",")
	for _, name := range pfnames {
		switch name {
		case "cpu":
			pfs = append(pfs, &wzprof.ProfilerCPU{
				Sampling: float32(prog.Sampling),
			})
		case "mem":
			pfs = append(pfs, &wzprof.ProfilerMemory{})
		}
	}

	//pl := wzprof.NewProfileListener(pfs...)
	pl := wzprof.NewProfileListener(
		&wzprof.ProfilerCPUTime{
			Sampling: float32(*sampling),
		},
		&wzprof.ProfilerCPU{
			Sampling: float32(*sampling),
		},
	)
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
		defer cancel()
		compiled, err := runtime.CompileModule(ctx, wasmCode)
		if err != nil {
			fmt.Println(err)
			return
		}
		pl.PrepareSymbols(compiled)

		defer func() {
			if err := compiled.Close(ctx); err != nil {
				fmt.Println(err)
			}
		}()

		instance, err := runtime.InstantiateModule(ctx, compiled, config)
		if err != nil {
			fmt.Println(err)
			return
		}

		if err := instance.Close(ctx); err != nil {
			fmt.Println(err)
		}
	}()

	if prog.HttpAddr != "" {
		go func() {
			if err := http.ListenAndServe(prog.HttpAddr, pl); err != nil {
				log.Println(err)
			}
		}()
	}

	<-ctx.Done()
	cancel()

	if prog.File != "" {
		if err := writeFile(prog.File, pl.BuildProfile()); err != nil {
			return err
		}
		fmt.Println("profile written to", prog.File)
	}

	return nil
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	var (
		file      = flag.String("file", "", "Filename to write profile to")
		httpAddr  = flag.String("http", "", "HTTP server address")
		sampling  = flag.Float64("sampling", defaultCPUSampling, "CPU sampling rate")
		profilers = flag.String("profilers", "cpu,mem", "Comma-separated list of profilers to use")
	)

	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		return fmt.Errorf("usage: wzprof </path/to/app.wasm>")
	}
	wasmPath := args[0]

	return program{
		WasmPath:  wasmPath,
		File:      *file,
		HttpAddr:  *httpAddr,
		Sampling:  *sampling,
		Profilers: *profilers,
	}.Execute(ctx)
}

func writeFile(fname string, p *profile.Profile) error {
	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer f.Close()

	return p.Write(f)
}
