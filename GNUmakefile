CC ?= gcc
CXX ?= g++
NVCC ?= nvcc
AR ?= ar

.DEFAULT_GOAL := build

obj:
	mkdir obj

obj/decred.dll: obj sph/blake.c decred.cu
	$(NVCC) --shared --compiler-options=-GS-,-MD -I. -Isph decred.cu sph/blake.c -o obj/decred.dll

build: obj/decred.dll
	go build

install: obj/cuda.a
	go install

clean:
	rm -rf obj
	go clean
