//go:build windows

package main

// Taking the sound out of a video with Windows' own Media Foundation: the
// decoders Windows already has (MP4 and M4A with AAC, MP3, WMA, WAV…), so
// nothing extra is downloaded. Windows decodes; speechWAV mixes and resamples.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mfplat                          = windows.NewLazySystemDLL("mfplat.dll")
	mfreadwrite                     = windows.NewLazySystemDLL("mfreadwrite.dll")
	mfOle32                         = windows.NewLazySystemDLL("ole32.dll")
	procMFStartup                   = mfplat.NewProc("MFStartup")
	procMFShutdown                  = mfplat.NewProc("MFShutdown")
	procMFCreateMediaType           = mfplat.NewProc("MFCreateMediaType")
	procMFCreateSourceReaderFromURL = mfreadwrite.NewProc("MFCreateSourceReaderFromURL")
	procMFCoInitializeEx            = mfOle32.NewProc("CoInitializeEx")
	procMFCoUninitialize            = mfOle32.NewProc("CoUninitialize")
)

var (
	mfMajorType  = windows.GUID{Data1: 0x48eba18e, Data2: 0xf8c9, Data3: 0x4687, Data4: [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	mfSubtype    = windows.GUID{Data1: 0xf7e34c9a, Data2: 0x42e8, Data3: 0x4714, Data4: [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	mfAudio      = windows.GUID{Data1: 0x73647561, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	mfPCM        = windows.GUID{Data1: 0x00000001, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	mfChannels   = windows.GUID{Data1: 0x37e48bf5, Data2: 0x645e, Data3: 0x4c5b, Data4: [8]byte{0x89, 0xde, 0xad, 0xa9, 0xe2, 0x9b, 0x69, 0x6a}}
	mfSampleRate = windows.GUID{Data1: 0x5faeeae7, Data2: 0x0290, Data3: 0x4c31, Data4: [8]byte{0x9e, 0x8a, 0xc5, 0x34, 0xf6, 0x8d, 0x9d, 0xba}}
	mfBits       = windows.GUID{Data1: 0xf2deb57f, Data2: 0x40fa, Data3: 0x4764, Data4: [8]byte{0xaa, 0x33, 0xed, 0x4f, 0x2d, 0x1f, 0xf6, 0x69}}
	mfDuration   = windows.GUID{Data1: 0x6c990d33, Data2: 0xbb8e, Data3: 0x477a, Data4: [8]byte{0x85, 0x98, 0x0d, 0x5d, 0x96, 0xfc, 0xd8, 0x8a}}
)

// COM method numbers (IUnknown, IMFAttributes, IMFSourceReader, IMFSample, IMFMediaBuffer)
const (
	comRelease          = 2
	attrGetUINT32       = 7
	attrSetUINT32       = 21
	attrSetGUID         = 24
	readerSetSelection  = 4
	readerGetCurrent    = 6
	readerSetCurrent    = 7
	readerReadSample    = 9
	readerGetPresAttr   = 12
	sampleContiguousBuf = 41
	bufferLock          = 3
	bufferUnlock        = 4

	mfAllStreams     = 0xFFFFFFFE
	mfFirstAudio     = 0xFFFFFFFD
	mfMediaSource    = 0xFFFFFFFF
	mfFlagError      = 0x1
	mfFlagEnd        = 0x2
	mfFlagTypeChange = 0x20
)

// comCall calls method i of a COM object.
//
//go:uintptrescapes
func comCall(obj unsafe.Pointer, i int, args ...uintptr) int32 {
	if obj == nil {
		return -1
	}
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(i)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return int32(r)
}

func decodeAudioOS(ctx context.Context, in, out string, progress func(float64)) error {
	runtime.LockOSThread() // COM belongs to the thread that started it
	defer runtime.UnlockOSThread()
	procMFCoInitializeEx.Call(0, 0) // COINIT_MULTITHREADED
	defer procMFCoUninitialize.Call()
	if hr, _, _ := procMFStartup.Call(0x00020070, 0); int32(hr) < 0 {
		return fmt.Errorf("Windows' media decoders didn't start (%#x)", uint32(hr))
	}
	defer procMFShutdown.Call()

	path, err := windows.UTF16PtrFromString(in)
	if err != nil {
		return err
	}
	var reader unsafe.Pointer
	if hr, _, _ := procMFCreateSourceReaderFromURL.Call(uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&reader))); int32(hr) < 0 {
		return fmt.Errorf("Windows can't open this kind of file (%#x)", uint32(hr))
	}
	defer comCall(reader, comRelease)
	comCall(reader, readerSetSelection, mfAllStreams, 0)
	if comCall(reader, readerSetSelection, mfFirstAudio, 1) < 0 {
		return errors.New("this file has no sound")
	}
	// decoded to 16-bit PCM, at the sound's own rate and channels
	var want unsafe.Pointer
	if hr, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&want))); int32(hr) < 0 {
		return fmt.Errorf("Windows' media decoders failed (%#x)", uint32(hr))
	}
	defer comCall(want, comRelease)
	comCall(want, attrSetGUID, uintptr(unsafe.Pointer(&mfMajorType)), uintptr(unsafe.Pointer(&mfAudio)))
	comCall(want, attrSetGUID, uintptr(unsafe.Pointer(&mfSubtype)), uintptr(unsafe.Pointer(&mfPCM)))
	comCall(want, attrSetUINT32, uintptr(unsafe.Pointer(&mfBits)), 16)
	if hr := comCall(reader, readerSetCurrent, mfFirstAudio, 0, uintptr(want)); hr < 0 {
		return fmt.Errorf("Windows has no decoder for this file's sound (%#x)", uint32(hr))
	}

	wav, err := newSpeechWAV(out)
	if err != nil {
		return err
	}
	readFormat := func() error {
		var cur unsafe.Pointer
		if comCall(reader, readerGetCurrent, mfFirstAudio, uintptr(unsafe.Pointer(&cur))) < 0 {
			return errors.New("the sound's format isn't known")
		}
		defer comCall(cur, comRelease)
		var ch, rate uint32
		comCall(cur, attrGetUINT32, uintptr(unsafe.Pointer(&mfChannels)), uintptr(unsafe.Pointer(&ch)))
		comCall(cur, attrGetUINT32, uintptr(unsafe.Pointer(&mfSampleRate)), uintptr(unsafe.Pointer(&rate)))
		if ch == 0 || rate == 0 {
			return errors.New("the sound's format isn't known")
		}
		wav.format(int(rate), int(ch))
		return nil
	}
	if err := readFormat(); err != nil {
		wav.abort()
		return err
	}
	var dur uint64                                                                                                                                                                // 100 ns units
	var pv [24]byte                                                                                                                                                               // PROPVARIANT
	if comCall(reader, readerGetPresAttr, mfMediaSource, uintptr(unsafe.Pointer(&mfDuration)), uintptr(unsafe.Pointer(&pv[0]))) >= 0 && binary.LittleEndian.Uint16(pv[:]) == 21 { // VT_UI8
		dur = binary.LittleEndian.Uint64(pv[8:])
	}
	for {
		if err := ctx.Err(); err != nil {
			wav.abort()
			return err
		}
		var idx, flags uint32
		var ts int64
		var sample unsafe.Pointer
		if hr := comCall(reader, readerReadSample, mfFirstAudio, 0, uintptr(unsafe.Pointer(&idx)), uintptr(unsafe.Pointer(&flags)), uintptr(unsafe.Pointer(&ts)), uintptr(unsafe.Pointer(&sample))); hr < 0 || flags&mfFlagError != 0 {
			wav.abort()
			return fmt.Errorf("Windows couldn't decode this file's sound (%#x)", uint32(hr))
		}
		if flags&mfFlagTypeChange != 0 {
			if err := readFormat(); err != nil {
				comCall(sample, comRelease)
				wav.abort()
				return err
			}
		}
		if sample != nil {
			var buf unsafe.Pointer
			if comCall(sample, sampleContiguousBuf, uintptr(unsafe.Pointer(&buf))) >= 0 {
				var p unsafe.Pointer
				var n uint32
				if comCall(buf, bufferLock, uintptr(unsafe.Pointer(&p)), 0, uintptr(unsafe.Pointer(&n))) >= 0 {
					if p != nil && n > 0 {
						wav.write(unsafe.Slice((*byte)(p), n))
					}
					comCall(buf, bufferUnlock)
				}
				comCall(buf, comRelease)
			}
			comCall(sample, comRelease)
		}
		if flags&mfFlagEnd != 0 {
			break
		}
		if dur > 0 && ts > 0 {
			progress(min(1, float64(ts)/float64(dur)))
		}
	}
	return wav.finish()
}
