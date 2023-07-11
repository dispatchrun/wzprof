.PHONY: all clean test testdata wasi-libc

testdata.c.src = $(wildcard testdata/c/*.c)
testdata.c.wasm = $(testdata.c.src:.c=.wasm)

testdata.go.src = $(wildcard testdata/go/*.go)
testdata.go.wasm = $(testdata.go.src:.go=.wasm)

testdata.tinygo.src = $(wildcard testdata/tinygo/*.go)
testdata.tinygo.wasm = $(testdata.tinygo.src:.go=.wasm)

testdata.wat.src = $(wildcard testdata/wat/*.go)
testdata.wat.wasm = $(testdata.wat.src:.wat=.wasm)

testdata.files = \
	$(testdata.c.wasm) \
	$(testdata.go.wasm) \
	$(testdata.tinygo.wasm) \
	$(testdata.wat.wasm)

python.files = .python/python.wasm .python/python311.zip

all: test

clean:
	rm -f $(testdata.files) $(python.files)

test: testdata
	go test ./...

testdata: wasi-libc python $(testdata.files)

testdata/.sysroot:
	mkdir -p testdata/.sysroot

testdata/.wasi-libc: testdata/.wasi-libc/.git

testdata/.wasi-libc/.git: .gitmodules
	git submodule update --init --recursive -- testdata/.wasi-libc

testdata/.sysroot/lib/wasm32-wasi/libc.a: testdata/.wasi-libc
	make -j4 -C testdata/.wasi-libc install INSTALL_DIR=../.sysroot

testdata/c/%.c: wasi-libc
testdata/c/%.wasm: testdata/c/%.c
	clang $< -o $@ -Wall -Os -target wasm32-unknown-wasi --sysroot testdata/.sysroot

testdata/go/%.wasm: testdata/go/%.go
	GOARCH=wasm GOOS=wasip1 gotip build -o $@ $<

testdata/tinygo/%.wasm: testdata/tinygo/%.go
	tinygo build -target=wasi -o $@ $<

testdata/wat/%.wasm: testdata/wat/%.wat
	wat2wasm -o $@ $<

wasi-libc: testdata/.sysroot/lib/wasm32-wasi/libc.a

python: $(python.files)

.python/python.wasm:
	mkdir -p $(dir $@)
	curl -fsSL https://timecraft.s3.amazonaws.com/python-vanilla/main/python.wasm -o $@

.python/python311.zip:
	mkdir -p $(dir $@)
	curl -fsSL https://timecraft.s3.amazonaws.com/python-vanilla/main/python311.zip -o $@

.gitmodules:
	git submodule add --name wasi-libc -- \
		'https://github.com/WebAssembly/wasi-libc' testdata/.wasi-libc
