module github.com/stealthrocket/wzprof

go 1.20

require (
	github.com/google/pprof v0.0.0-20230406165453-00490a63f317
	github.com/tetratelabs/wazero v1.0.3
)

require golang.org/x/exp v0.0.0-20230425010034-47ecfdc1ba53

replace github.com/tetratelabs/wazero => github.com/stealthrocket/wazero v0.0.0-20230506195512-778fba8a2815
