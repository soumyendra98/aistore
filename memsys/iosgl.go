// Package memsys provides memory management and slab/SGL allocation with io.Reader and io.Writer interfaces
// on top of scatter-gather lists of reusable buffers.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package memsys

import (
	"errors"
	"io"
	"sync"

	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
)

// interface guard
var (
	_ cos.WriterAt       = (*SGL)(nil)
	_ io.ReaderFrom      = (*SGL)(nil)
	_ io.WriterTo        = (*SGL)(nil)
	_ cos.ReadOpenCloser = (*SGL)(nil)
	_ cos.ReadOpenCloser = (*Reader)(nil)
)

type (
	// implements io.ReadWriteCloser + Reset
	SGL struct {
		sgl  [][]byte
		slab *Slab
		woff int64 // stream
		roff int64
	}
	// uses the underlying SGL to implement io.ReadWriteCloser + io.Seeker
	Reader struct {
		z    *SGL
		roff int64
	}
)

/////////////
// sglPool //
/////////////

var (
	sglPool sync.Pool
	sgl0    SGL
)

func _allocSGL() (z *SGL) {
	if v := sglPool.Get(); v != nil {
		z = v.(*SGL)
	} else {
		z = &SGL{}
	}
	return
}

func _freeSGL(z *SGL) {
	sgl := z.sgl[:0]
	*z = sgl0
	z.sgl = sgl
	sglPool.Put(z)
}

// SGL implements io.ReadWriteCloser  + Reset (see https://golang.org/pkg/io/#ReadWriteCloser)
//
// SGL grows "automatically" and on demand upon writing.
// The package does not provide any mechanism to limit the sizes
// of allocated slabs or to react on memory pressure by dynamically shrinking slabs
// at runtime. The responsibility to call sgl.Reclaim (see below) lies with the user.

func (z *SGL) Cap() int64  { return int64(len(z.sgl)) * z.slab.Size() }
func (z *SGL) Size() int64 { return z.woff }
func (z *SGL) Slab() *Slab { return z.slab }
func (z *SGL) IsNil() bool { return z == nil || z.slab == nil }

func (z *SGL) grow(toSize int64) {
	z.slab.muget.Lock()
	for z.Cap() < toSize {
		z.sgl = append(z.sgl, z.slab._alloc())
	}
	z.slab.muget.Unlock()
}

func (z *SGL) ReadFrom(r io.Reader) (n int64, err error) {
	for {
		if z.woff-z.Cap() == 0 {
			z.grow(z.Cap() + z.slab.Size())
		}

		idx := z.woff / z.slab.Size()
		off := z.woff % z.slab.Size()
		buf := z.sgl[idx][off:]

		written, err := r.Read(buf)
		z.woff += int64(written)
		n += int64(written)
		if err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
	}
}

// NOTE: not advancing roff here - see usage
func (z *SGL) WriteTo(dst io.Writer) (n int64, err error) {
	var (
		n0      int
		toWrite = z.woff
	)
	for _, buf := range z.sgl {
		l := cos.MinI64(toWrite, int64(len(buf)))
		if l == 0 {
			break
		}
		n0, err = dst.Write(buf[:l])
		n += int64(n0)
		toWrite -= l

		if err != nil {
			return
		}
	}
	return
}

func (z *SGL) Write(p []byte) (n int, err error) {
	wlen := len(p)
	if needtot := z.woff + int64(wlen); needtot > z.Cap() {
		z.grow(needtot)
	}
	idx, off, poff := z.woff/z.slab.Size(), z.woff%z.slab.Size(), 0
	for wlen > 0 {
		size := cos.MinI64(z.slab.Size()-off, int64(wlen))
		buf := z.sgl[idx]
		src := p[poff : poff+int(size)]
		copy(buf[off:], src)
		z.woff += size
		idx++
		off = 0
		wlen -= int(size)
		poff += int(size)
	}
	return len(p), nil
}

func (z *SGL) WriteByte(c byte) error {
	if needtot := z.woff + 1; needtot > z.Cap() {
		z.grow(needtot)
	}
	idx, off := z.woff/z.slab.Size(), z.woff%z.slab.Size()
	buf := z.sgl[idx]
	buf[off] = c
	z.woff++
	return nil
}

func (z *SGL) Read(b []byte) (n int, err error) {
	n, z.roff, err = z.readAtOffset(b, z.roff)
	return
}

func (z *SGL) readAtOffset(b []byte, roffin int64) (n int, roff int64, err error) {
	roff = roffin
	if roff >= z.woff {
		err = io.EOF
		return
	}
	idx, off := int(roff/z.slab.Size()), roff%z.slab.Size()
	buf := z.sgl[idx]
	size := cos.MinI64(int64(len(b)), z.woff-roff)
	n = copy(b[:size], buf[off:])
	roff += int64(n)
	for n < len(b) && idx < len(z.sgl)-1 {
		idx++
		buf = z.sgl[idx]
		size = cos.MinI64(int64(len(b)-n), z.woff-roff)
		n1 := copy(b[n:n+int(size)], buf)
		roff += int64(n1)
		n += n1
	}
	if n < len(b) {
		err = io.EOF
	}
	return
}

// ReadAll is a convenience method and an optimized alternative to the generic
// io.ReadAll. Similarly to the latter, a successful call returns err == nil,
// not err == EOF. The difference, though, is that the method always succeeds.
// NOTE: intended usage includes testing code and debug.
func (z *SGL) ReadAll() (b []byte, err error) {
	b = make([]byte, z.Size())
	for off, i := 0, 0; i < len(z.sgl); i++ {
		n := copy(b[off:], z.sgl[i])
		off += n
	}
	return
}

// NOTE: Not fully implemented use carefully!
func (z *SGL) WriteAt(p []byte, off int64) (n int, err error) {
	debug.Assert(z.woff >= off+int64(len(p)))

	prevWriteOff := z.woff
	z.woff = off
	n, err = z.Write(p)
	z.woff = prevWriteOff
	return n, err
}

// reuse already allocated SGL
func (z *SGL) Reset()                            { z.woff, z.roff = 0, 0 }
func (z *SGL) Len() int64                        { return z.woff - z.roff }
func (z *SGL) Open() (cos.ReadOpenCloser, error) { return NewReader(z), nil }
func (*SGL) Close() error                        { return nil }

func (z *SGL) Free() {
	debug.Assert(z.slab != nil)
	s := z.slab
	s.muput.Lock()
	for _, buf := range z.sgl {
		size := cap(buf)
		debug.Assert(int64(size) == s.Size())
		b := buf[:size] // always freeing original (fixed buffer) size
		deadbeef(b)
		s.put = append(s.put, b)
	}
	s.muput.Unlock()
	_freeSGL(z)
}

func (z *SGL) Bytes() (b []byte) {
	cos.Assert(z.roff == 0) // NOTE: done for existing limited use case
	if z.woff >= z.slab.Size() {
		b, _ = z.ReadAll()
		return
	}
	return z.sgl[0][:z.woff]
}

//
// SGL Reader - implements io.ReadWriteCloser + io.Seeker
// A given SGL can be simultaneously utilized by multiple Readers
//

func NewReader(z *SGL) *Reader                      { return &Reader{z, 0} }
func (r *Reader) Open() (cos.ReadOpenCloser, error) { return NewReader(r.z), nil }
func (*Reader) Close() error                        { return nil }

func (r *Reader) Read(b []byte) (n int, err error) {
	n, r.roff, err = r.z.readAtOffset(b, r.roff)
	return
}

func (r *Reader) Seek(from int64, whence int) (offset int64, err error) {
	switch whence {
	case io.SeekStart:
		offset = from
	case io.SeekCurrent:
		offset = r.roff + from
	case io.SeekEnd:
		offset = r.z.woff + from
	default:
		return 0, errors.New("invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	r.roff = offset
	return
}
