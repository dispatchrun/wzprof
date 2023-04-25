module github.com/stealthrocket/wazero-profiler

go 1.20

require (
	github.com/cespare/xxhash v1.1.0
	github.com/google/pprof v0.0.0-20230406165453-00490a63f317
	github.com/tetratelabs/wazero v1.0.3
)

replace github.com/tetratelabs/wazero => github.com/stealthrocket/wazero v0.0.0-20230414193015-6c5b62c73c68
