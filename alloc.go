package smux

import (
	"errors"
	"sync"
)

// Allocator reused buffer for incoming frames.
type Allocator interface {
	// Get returns size length of buffer.
	Get(size int) ([]byte, error)
	// Put back buffer to allocator.
	Put([]byte) error
	// Registered read/write on registered memory.
	Registered() bool
}

var (
	defaultAllocator Allocator
	debruijinPos     = [...]byte{
		0, 9, 1, 10, 13, 21, 2, 29, 11, 14,
		16, 18, 22, 25, 3, 30, 8, 12, 20, 28,
		15, 17, 24, 7, 19, 27, 23, 6, 26, 5, 4, 31,
	}
)

const maxsize = 1 << 24

func init() {
	defaultAllocator = NewAllocator()
}

// allocator for incoming frames, optimized to prevent overwriting after zeroing
type allocator struct {
	buffers []sync.Pool
}

// NewAllocator initiates a []byte allocator for frames less than 65536 bytes,
// the waste(memory fragmentation) of space allocation is guaranteed to be
// no more than 50%.
func NewAllocator() Allocator {
	alloc := new(allocator)
	alloc.buffers = make([]sync.Pool, 25) // 1B -> 16MB
	for k := range alloc.buffers {
		i := k
		alloc.buffers[k].New = func() interface{} {
			return make([]byte, 1<<uint32(i))
		}
	}
	return alloc
}

// Get a []byte from pool with most appropriate cap
func (alloc *allocator) Get(size int) ([]byte, error) {
	if size <= 0 || size > maxsize {
		return nil, ErrAllocOversize
	}

	bits := msb(size)
	if size == 1<<bits {
		return alloc.buffers[bits].Get().([]byte)[:size], nil
	}
	return alloc.buffers[bits+1].Get().([]byte)[:size], nil
}

// Put returns a []byte to pool for future use,
// which the cap must be exactly 2^n
func (alloc *allocator) Put(buf []byte) error {
	bits := msb(cap(buf))
	if cap(buf) == 0 || cap(buf) > maxsize || cap(buf) != 1<<bits {
		return errors.New("allocator Put() incorrect buffer size")
	}
	alloc.buffers[bits].Put(buf) // nolint: staticcheck
	return nil
}

func (alloc *allocator) Registered() bool { return false }

// msb return the pos of most significiant bit
// http://supertech.csail.mit.edu/papers/debruijn.pdf
func msb(size int) byte {
	v := uint32(size)
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	return debruijinPos[(v*0x07C4ACDD)>>27]
}
