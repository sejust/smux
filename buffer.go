package smux

import "sync"

type ringbuffer struct {
	lock    sync.Mutex
	index   int // to read the index
	offset  int // offset of the indexed data
	next    int // next empty place, -1 means to grow
	buffers [][]byte
	alloc   Allocator // put buffer back to Allocator
}

func newRingbuffer(alloc Allocator) *ringbuffer {
	return &ringbuffer{next: -1, alloc: alloc}
}

func (r *ringbuffer) Enqueue(p []byte) {
	r.lock.Lock()
	if r.next >= 0 {
		r.buffers[r.next] = p
		r.next = (r.next + 1) % len(r.buffers)
		if r.buffers[r.next] != nil {
			r.next = -1
		}
		r.lock.Unlock()
		return
	}

	buffers := append(r.buffers, p) // runtime.growslice
	n := copy(buffers, r.buffers[r.index:])
	copy(buffers[n:], r.buffers[0:r.index])
	r.buffers = buffers
	r.index = 0

	if len(r.buffers) < cap(r.buffers) {
		r.next = len(r.buffers)
		r.buffers = r.buffers[:cap(r.buffers)]
	}
	r.lock.Unlock()
}

func (r *ringbuffer) Read(p []byte) (n int) {
	r.lock.Lock()
	if len(r.buffers) == 0 {
		r.lock.Unlock()
		return
	}
	putback := false
	buff := r.buffers[r.index]
	if buff != nil {
		n = copy(p, buff[r.offset:])
		r.offset += n
		if r.offset == len(buff) {
			r.buffers[r.index] = nil
			if r.next == -1 {
				r.next = r.index
			}
			r.index = (r.index + 1) % len(r.buffers)
			r.offset = 0
			putback = true
		}
	}
	r.lock.Unlock()
	if putback {
		r.alloc.Put(buff)
	}
	return
}

func (r *ringbuffer) Dequeue() (p []byte, reuse func()) {
	r.lock.Lock()
	if len(r.buffers) == 0 {
		r.lock.Unlock()
		return
	}
	if buff := r.buffers[r.index]; buff != nil {
		reuse = func() { r.alloc.Put(buff) }
		p = buff[r.offset:]
		r.buffers[r.index] = nil
		if r.next == -1 {
			r.next = r.index
		}
		r.index = (r.index + 1) % len(r.buffers)
		r.offset = 0
	}
	r.lock.Unlock()
	return
}

func (r *ringbuffer) Recycle() (n int) {
	r.lock.Lock()
	for idx := range r.buffers {
		if buff := r.buffers[idx]; buff != nil {
			n += len(buff)
			if idx == r.index {
				n -= r.offset
				r.index = 0
				r.offset = 0
			}
			r.buffers[idx] = nil
			r.alloc.Put(buff)
		}
		r.next = 0
	}
	r.lock.Unlock()
	return
}

func (r *ringbuffer) HasData() (has bool) {
	r.lock.Lock()
	for idx := range r.buffers {
		if r.buffers[idx] != nil {
			has = true
			break
		}
	}
	r.lock.Unlock()
	return
}
