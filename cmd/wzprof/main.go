package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/google/pprof/profile"
	flag "github.com/spf13/pflag"
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
	Mounts    []string
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
		case "mem":
			pfs = append(pfs, &wzprof.ProfilerMemory{})
		case "cpu":
			pfs = append(pfs, &wzprof.ProfilerCPU{
				Sampling: float32(prog.Sampling),
			})
		case "cputime":
			pfs = append(pfs, &wzprof.ProfilerCPUTime{
				Sampling: float32(*sampling),
			})
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
		WithArgs(wasmName).
		WithFSConfig(createFSConfig(*mounts))

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
		mounts    = flag.StringSlice("mount", []string{}, "Comma-separated list of directories to mount (e.g. /tmp:/tmp:ro)")
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
		Mounts:    *mounts,
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
