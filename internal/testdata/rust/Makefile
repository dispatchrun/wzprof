all: simple/target/wasm32-wasi/debug/simple.wasm

simple/target/wasm32-wasi/debug/simple.wasm:
	cd simple && cargo build --target wasm32-wasi

.PHONY: clean
clean:
	rm -f simple/target/wasm32-wasi/debug/simple.wasm
