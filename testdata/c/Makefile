CC="${WASI_SDK_PATH}/bin/clang" "--sysroot=${WASI_SDK_PATH}/share/wasi-sysroot"

sources = $(wildcard *.c)
objects = $(sources:.c=.wasm)

all: $(objects)

%.wasm: %.c
	$(CC) --target=wasm32-unknown-wasi -g $? -o $@
