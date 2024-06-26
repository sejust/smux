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
	ring := newRingbuffer()

	if n := ring.Read(make([]byte, 8)); n != 0 {
		t.Fatal("empty read")
	}
	if buff, reuse := ring.Dequeue(); buff != nil || reuse != nil {
		t.Fatal("empty pop")
	}
	if has := ring.HasData(); has {
		t.Fatal("empty has data")
	}

	ring.Enqueue(defaultAllocator.Get(64))
	ring.Enqueue(defaultAllocator.Get(64))
	ring.Enqueue(defaultAllocator.Get(64))
	ring.Enqueue(defaultAllocator.Get(64))
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
	ring := newRingbuffer()
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
			defaultAllocator.Put(b.heads[0])
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
	buff := &testBuffers{}
	const n = 128
	b.SetBytes(64 * n)
	for i := 0; i < b.N; i++ {
		for range [n]struct{}{} {
			buff.append(defaultAllocator.Get(64))
			buff.read(r)
			buff.read(r)
		}
	}
}

func BenchmarkBufferRing(b *testing.B) {
	r := make([]byte, 32)
	ring := newRingbuffer()
	const n = 128
	b.SetBytes(64 * n)
	for i := 0; i < b.N; i++ {
		for range [n]struct{}{} {
			ring.Enqueue(defaultAllocator.Get(64))
			ring.Read(r)
			ring.Read(r)
		}
	}
}
