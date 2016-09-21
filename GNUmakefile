CC ?= gcc
CXX ?= g++
NVCC ?= nvcc
AR ?= ar
ARCH:=$(shell uname -s)

.DEFAULT_GOAL := build

obj:
	mkdir obj

ifeq ($(ARCH),Windows)
obj/decred.dll: obj sph/blake.c decred.cu
	$(NVCC) --shared --optimize=3 -I. --compiler-options=-GS-,-MD -Isph decred.cu sph/blake.c -o obj/decred.dll
else
obj/decred.a: obj sph/blake.c decred.cu
	$(NVCC) --lib --optimize=3 -I. decred.cu sph/blake.c -o obj/decred.a
endif

ifeq ($(ARCH),Windows)
build: obj/decred.dll
else
build: obj/decred.a
endif
	go build -tags 'cuda'

ifeq ($(ARCH),Windows)
install: obj/decred.dll
else
install: obj/decred.a
endif
	go install -tags 'cuda'

clean:
	rm -rf obj
	go clean
