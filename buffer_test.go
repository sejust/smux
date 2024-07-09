package smux

import (
	"crypto/rand"
	"hash/crc32"
	mrand "math/rand"
	"sync"
	"testing"
	"time"
)

func TestRingbufferBase(t *testing.T) {
	ring := newRingbuffer(defaultAllocator)

	if n := ring.Read(make([]byte, 8)); n != 0 {
		t.Fatal("empty read")
	}
	if buff, reuse := ring.Dequeue(); buff != nil || reuse != nil {
		t.Fatal("empty pop")
	}
	if has := ring.HasData(); has {
		t.Fatal("empty has data")
	}

	getBuffer := func() []byte {
		b, _ := defaultAllocator.Get(64)
		return b
	}
	ring.Enqueue(getBuffer())
	ring.Enqueue(getBuffer())
	ring.Enqueue(getBuffer())
	ring.Enqueue(getBuffer())
	buf := make([]byte, 32)
	if n := ring.Read(buf); n != 32 {
		t.Fatal("read half")
	}
	b, reuse := ring.Dequeue()
	if len(b) != 32 {
		t.Fatal("pop half")
	}
	reuse()
	if n := ring.Read(make([]byte, 128)); n != 64 {
		t.Fatal("read once")
	}
	if n := ring.Read(buf); n != 32 {
		t.Fatal("read half")
	}
	if has := ring.HasData(); !has {
		t.Fatal("has data before recycle")
	}
	if n := ring.Recycle(); n != 32+64 {
		t.Fatal("recycle")
	}
	if has := ring.HasData(); has {
		t.Fatal("has data after recycle")
	}
}

func TestRingbufferData(t *testing.T) {
	ring := newRingbuffer(defaultAllocator)
	newBuffer := func() []byte {
		return make([]byte, mrand.Intn(32<<10)+(32<<10))
	}

	run := func() {
		var wg sync.WaitGroup
		wg.Add(1)

		wcrc := crc32.NewIEEE()
		rcrc := crc32.NewIEEE()
		size := mrand.Intn(4<<20) + (2 << 20)

		go func(rsize int) {
			defer wg.Done()
			for rsize > 0 {
				b := newBuffer()
				n := ring.Read(b)
				if n == 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				rcrc.Write(b[:n])
				rsize -= n
			}
		}(size)

		for size > 0 {
			b := newBuffer()
			if size < len(b) {
				b = b[:size]
			}
			rand.Read(b)
			ring.Enqueue(b)
			wcrc.Write(b)
			size -= len(b)
		}

		wg.Wait()
		if wcrc.Sum32() != rcrc.Sum32() {
			t.Fatal("write read data mismatch")
		}
	}
	for range [100]struct{}{} {
		run()
	}
}

type testBuffers struct {
	lock    sync.Mutex
	buffers [][]byte
	heads   [][]byte
}

func (b *testBuffers) read(p []byte) {
	b.lock.Lock()
	if len(b.buffers) > 0 {
		n := copy(p, b.buffers[0])
		b.buffers[0] = b.buffers[0][n:]
		if len(b.buffers[0]) == 0 {
			b.buffers = b.buffers[1:]
			b.heads = b.heads[1:]
		}
	}
	b.lock.Unlock()
}

func (b *testBuffers) append(p []byte) {
	b.lock.Lock()
	b.buffers = append(b.buffers, p)
	b.heads = append(b.heads, p)
	b.lock.Unlock()
}

func BenchmarkBufferSlice(b *testing.B) {
	r := make([]byte, 32)
	w := make([]byte, 64)
	buff := &testBuffers{}
	const n = 128
	b.SetBytes(64 * n)
	for i := 0; i < b.N; i++ {
		for range [n]struct{}{} {
			buff.append(w)
			buff.read(r)
			buff.read(r)
		}
	}
}

type mockAlloc struct{}

func (mockAlloc) Get(size int) ([]byte, error) { return nil, nil }
func (mockAlloc) Put([]byte) error             { return nil }
func (mockAlloc) Registered() bool             { return false }

func BenchmarkBufferRing(b *testing.B) {
	r := make([]byte, 32)
	w := make([]byte, 64)
	ring := newRingbuffer(mockAlloc{})
	const n = 128
	b.SetBytes(64 * n)
	for i := 0; i < b.N; i++ {
		for range [n]struct{}{} {
			ring.Enqueue(w)
			ring.Read(r)
			ring.Read(r)
		}
	}
}
