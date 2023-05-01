CC="${WASI_SDK_PATH}/bin/clang" "--sysroot=${WASI_SDK_PATH}/share/wasi-sysroot"


all: simple.wasm

simple.wasm: simple.c
	$(CC) --target=wasm32-unknown-wasi -g $? -o $@
