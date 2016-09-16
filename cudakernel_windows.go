// Copyright (c) 2016 The Decred developers.

package main

import (
	"github.com/mumax/3/cuda/cu"
	"syscall"
	"unsafe"
)

var (
	kernelDll           = syscall.MustLoadDLL("decred.dll")
	precomputeTableProc = kernelDll.MustFindProc("decred_cpu_setBlock_52")
	kernelProc          = kernelDll.MustFindProc("decred_hash_nonce")
)

func cudaPrecomputeTable(input *[192]byte) {
	precomputeTableProc.Call(uintptr(unsafe.Pointer(input)))
}

func cudaInvokeKernel(gridx, blockx, threads uint32, startNonce uint32, nonceResults cu.DevicePtr, targetHigh uint32) {
	kernelProc.Call(uintptr(gridx), uintptr(blockx), uintptr(threads), uintptr(startNonce),
		uintptr(nonceResults), uintptr(targetHigh))
}
