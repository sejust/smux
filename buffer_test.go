package smux

import (
	"sync"
	"testing"
)

func TestRingbuffer(t *testing.T) {
	ring := newRingbuffer()

	if n := ring.read(make([]byte, 8)); n != 0 {
		t.Fatal("empty read")
	}
	if buff, reuse := ring.pop(); buff != nil || reuse != nil {
		t.Fatal("empty pop")
	}
	if has := ring.hasData(); has {
		t.Fatal("empty has data")
	}

	ring.append(defaultAllocator.Get(64))
	ring.append(defaultAllocator.Get(64))
	ring.append(defaultAllocator.Get(64))
	ring.append(defaultAllocator.Get(64))
	buf := make([]byte, 32)
	if n := ring.read(buf); n != 32 {
		t.Fatal("read half")
	}
	b, reuse := ring.pop()
	if len(b) != 32 {
		t.Fatal("pop half")
	}
	reuse()
	if n := ring.read(make([]byte, 128)); n != 64 {
		t.Fatal("read once")
	}
	if n := ring.read(buf); n != 32 {
		t.Fatal("read half")
	}
	if has := ring.hasData(); !has {
		t.Fatal("has data before recycle")
	}
	if n := ring.recycle(); n != 32+64 {
		t.Fatal("recycle")
	}
	if has := ring.hasData(); has {
		t.Fatal("has data after recycle")
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
	b.SetBytes(64)
	for i := 0; i < b.N; i++ {
		buff.append(defaultAllocator.Get(64))
		buff.read(r)
		buff.read(r)
	}
}

func BenchmarkBufferRing(b *testing.B) {
	r := make([]byte, 32)
	ring := newRingbuffer()
	b.SetBytes(64)
	for i := 0; i < b.N; i++ {
		ring.append(defaultAllocator.Get(64))
		ring.read(r)
		ring.read(r)
	}
}
