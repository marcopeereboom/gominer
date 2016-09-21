// Copyright (c) 2016 The Decred developers.

// +build cuda,!opencl

package main

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/mumax/3/cuda/cu"

	"github.com/decred/gominer/util"
	"github.com/decred/gominer/work"
)

const (
	// From ccminer
	threadsPerBlock = 640
	blockx          = threadsPerBlock
)

// Return the GPU library in use.
func gpuLib() string {
	return "Cuda"
}

const (
	localWorksize      = 64
	cuOutputBufferSize = 64
)

type Device struct {
	// The following variables must only be used atomically.
	fanPercent  uint32
	temperature uint32

	sync.Mutex
	index int
	cuda  bool

	deviceName    string
	fanTempActive bool
	kind          string

	// Items for CUDA device
	cuDeviceID cu.Device
	cuContext  cu.Context
	//cuInput        cu.DevicePtr
	cuInSize       int64
	cuOutputBuffer []float64

	workSize uint32

	// extraNonce is the device extraNonce, where the first
	// byte is the device ID (supporting up to 255 devices)
	// while the last 3 bytes is the extraNonce value. If
	// the extraNonce goes through all 0x??FFFFFF values,
	// it will reset to 0x??000000.
	extraNonce    uint32
	currentWorkID uint32

	midstate  [8]uint32
	lastBlock [16]uint32

	work     work.Work
	newWork  chan *work.Work
	workDone chan []byte
	hasWork  bool

	started          uint32
	allDiffOneShares uint64
	validShares      uint64
	invalidShares    uint64

	quit chan struct{}
}

func decredCPUSetBlock52(input *[192]byte) {
	if input == nil {
		panic("input is nil")
	}
	C.decred_cpu_setBlock_52((*C.uint32_t)(unsafe.Pointer(input)))
}

func decredHashNonce(gridx, blockx, threads uint32, startNonce uint32, nonceResults cu.DevicePtr, targetHigh uint32) {
	C.decred_hash_nonce(C.uint32_t(gridx), C.uint32_t(blockx), C.uint32_t(threads),
		C.uint32_t(startNonce), (*C.uint32_t)(unsafe.Pointer(nonceResults)), C.uint32_t(targetHigh))
}

func deviceInfo(index int) (uint32, uint32) {
	fanPercent := uint32(0)
	temperature := uint32(0)

	return fanPercent, temperature
}

func getInfo() ([]cu.Device, error) {
	cu.Init(0)
	ids := cu.DeviceGetCount()
	minrLog.Infof("%v GPUs", ids)
	var CUdevices []cu.Device
	for i := 0; i < ids; i++ {
		dev := cu.DeviceGet(i)
		CUdevices = append(CUdevices, dev)
		minrLog.Infof("%v: %v", i, dev.Name())
	}
	return CUdevices, nil
}

// getCUDevices returns the list of devices for the given platform.
func getCUDevices() ([]cu.Device, error) {
	cu.Init(0)

	version := cu.Version()
	fmt.Println(version)

	maj := version / 1000
	min := version % 100

	minMajor := 5
	minMinor := 5

	if maj < minMajor || (maj == minMajor && min < minMinor) {
		return nil, fmt.Errorf("Driver does not support CUDA %v.%v API", minMajor, minMinor)
	}

	var numDevices int
	numDevices = cu.DeviceGetCount()
	if numDevices < 1 {
		return nil, fmt.Errorf("No devices found")
	}
	devices := make([]cu.Device, numDevices)
	for i := 0; i < numDevices; i++ {
		dev := cu.DeviceGet(i)
		devices[i] = dev
	}
	return devices, nil
}

// ListDevices prints a list of CUDA capable GPUs present.
func ListDevices() {
	// CUDA devices
	// Because mumux3/3/cuda/cu likes to panic instead of error.
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("No CUDA Capable GPUs present")
		}
	}()
	devices, _ := getCUDevices()
	for i, dev := range devices {
		fmt.Printf("CUDA Capable GPU #%d: %s\n", i, dev.Name())
	}
}

func NewCuDevice(index int, order int, deviceID cu.Device,
	workDone chan []byte) (*Device, error) {

	d := &Device{
		index:       index,
		cuDeviceID:  deviceID,
		deviceName:  deviceID.Name(),
		cuda:        true,
		kind:        "nvidia",
		quit:        make(chan struct{}),
		newWork:     make(chan *work.Work, 5),
		workDone:    workDone,
		fanPercent:  0,
		temperature: 0,
	}

	d.cuInSize = 21

	fanPercent, temperature := deviceInfo(d.index)
	// Newer cards will idle with the fan off so just check if we got
	// a good temperature reading
	if temperature != 0 {
		atomic.StoreUint32(&d.fanPercent, fanPercent)
		atomic.StoreUint32(&d.temperature, temperature)
		d.fanTempActive = true
	}

	d.started = uint32(time.Now().Unix())

	// Autocalibrate?

	return d, nil
}

func (d *Device) runDevice() error {
	// Bump the extraNonce for the device it's running on
	// when you begin mining. This ensures each GPU is doing
	// different work. If the extraNonce has already been
	// set for valid work, restore that.
	d.extraNonce += uint32(d.index) << 24
	d.lastBlock[work.Nonce1Word] = util.Uint32EndiannessSwap(d.extraNonce)

	// Need to have this stuff here for a ctx vs thread issue.
	runtime.LockOSThread()

	// Create the CU context
	d.cuContext = cu.CtxCreate(cu.CTX_BLOCKING_SYNC, d.cuDeviceID)

	// Allocate the input region
	d.cuContext.SetCurrent()

	// kernel is built with nvcc, not an api call so must be done
	// at compile time.

	minrLog.Infof("Started GPU #%d: %s", d.index, d.deviceName)
	nonceResultsH := cu.MemAllocHost(d.cuInSize * 4)
	nonceResultsD := cu.MemAlloc(d.cuInSize * 4)
	defer cu.MemFreeHost(nonceResultsH)
	defer nonceResultsD.Free()

	nonceResultsHSliceHeader := reflect.SliceHeader{
		Data: uintptr(nonceResultsH),
		Len:  int(d.cuInSize),
		Cap:  int(d.cuInSize),
	}
	nonceResultsHSlice := *(*[]uint32)(unsafe.Pointer(&nonceResultsHSliceHeader))

	endianData := new([192]byte)

	for {
		d.updateCurrentWork()

		select {
		case <-d.quit:
			return nil
		default:
		}

		// Increment extraNonce.
		util.RolloverExtraNonce(&d.extraNonce)
		d.lastBlock[work.Nonce1Word] = util.Uint32EndiannessSwap(d.extraNonce)

		copy(endianData[:], d.work.Data[:128])
		for i, j := 128, 0; i < 180; {
			b := make([]byte, 4)
			binary.BigEndian.PutUint32(b, d.lastBlock[j])
			copy(endianData[i:], b)
			i += 4
			j++
		}
		cudaPrecomputeTable(endianData)

		// Update the timestamp. Only solo work allows you to roll
		// the timestamp.
		ts := d.work.JobTime
		if d.work.IsGetWork {
			diffSeconds := uint32(time.Now().Unix()) - d.work.TimeReceived
			ts = d.work.JobTime + diffSeconds
		}
		d.lastBlock[work.TimestampWord] = util.Uint32EndiannessSwap(ts)

		// Only zero the first item which contains the number of results
		nonceResultsHSlice[0] = 0
		cu.MemcpyHtoD(nonceResultsD, nonceResultsH, 4)

		// Execute the kernel and follow its execution time.
		currentTime := time.Now()

		startNonce := d.lastBlock[work.Nonce1Word]

		throughput := uint32(0x20000000)
		gridx := ((throughput - 1) / 640)

		gridx = 52428 // like ccminer

		targetHigh := ^uint32(0)

		cudaInvokeKernel(gridx, blockx, throughput, startNonce, nonceResultsD, targetHigh)

		// Copy just the number of results here.  The actual results are
		// copied after this is known.  ccminer does this to only copy
		// the memory that is needed instead of the entire array.  When
		// there are no results the second copy can be skipped.
		//cu.MemcpyDtoH(nonceResultsH, nonceResultsD, 4)
		//numResults := nonceResultsHSlice[0]
		//if numResults != 0 {
		//	cu.MemcpyDtoH(nonceResultsHResOffset, nonceResultsDResOffset, 4*int64(numResults))
		//}
		cu.MemcpyDtoH(nonceResultsH, nonceResultsD, d.cuInSize)

		for i, result := range nonceResultsHSlice[1 : 1+numResults] {
			// lol seelog
			i := i
			result := result
			minrLog.Debugf("GPU #%d: Found candidate %v nonce %08x, "+
				"extraNonce %08x, workID %08x, timestamp %08x",
				d.index, i, result, d.lastBlock[work.Nonce1Word],
				util.Uint32EndiannessSwap(d.currentWorkID),
				d.lastBlock[work.TimestampWord])

			// Assess the work. If it's below target, it'll be rejected
			// here. The mining algorithm currently sends this function any
			// difficulty 1 shares.
			d.foundCandidate(d.lastBlock[work.TimestampWord], result,
				d.lastBlock[work.Nonce1Word])
		}

		elapsedTime := time.Since(currentTime)
		minrLog.Tracef("GPU #%d: Kernel execution to read time: %v", d.index,
			elapsedTime)
	}
}

func minUint32(a, b uint32) uint32 {
	if a > b {
		return a
	} else {
		return b
	}
}

func newMinerDevs(m *Miner) (*Miner, int, error) {
	deviceListIndex := 0
	deviceListEnabledCount := 0

	CUdeviceIDs, err := getInfo()
	if err != nil {
		return nil, 0, err
	}

	// XXX Can probably combine these bits with the opencl ones once
	// I decide what to do about the types.

	for _, CUDeviceID := range CUdeviceIDs {
		miningAllowed := false

		// Enforce device restrictions if they exist
		if len(cfg.DeviceIDs) > 0 {
			for _, i := range cfg.DeviceIDs {
				if deviceListIndex == i {
					miningAllowed = true
				}
			}
		} else {
			miningAllowed = true
		}

		if miningAllowed {
			newDevice, err := NewCuDevice(deviceListIndex, deviceListEnabledCount, CUDeviceID, m.workDone)
			deviceListEnabledCount++
			m.devices = append(m.devices, newDevice)
			if err != nil {
				return nil, 0, err
			}
		}
		deviceListIndex++
	}

	return m, deviceListEnabledCount, nil
}

func (d *Device) Release() {
	d.cuContext.SetCurrent()
	//d.cuInput.Free()
	cu.CtxDestroy(&d.cuContext)
}
