package smux

import "sync"

type ringbuffer struct {
	lock    sync.Mutex
	index   int
	offset  int
	buffers [][]byte
}

func newRingbuffer() *ringbuffer {
	return &ringbuffer{}
}

func (r *ringbuffer) append(b []byte) {
	r.lock.Lock()
	for ii := range r.buffers {
		idx := (r.index + ii) % len(r.buffers)
		if r.buffers[idx] == nil {
			r.buffers[idx] = b
			r.lock.Unlock()
			return
		}
	}
	r.buffers = append(r.buffers, b)
	r.buffers = r.buffers[:cap(r.buffers)]
	r.lock.Unlock()
}

func (r *ringbuffer) read(p []byte) (n int) {
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
			r.index = (r.index + 1) % len(r.buffers)
			r.offset = 0
			putback = true
		}
	}
	r.lock.Unlock()
	if putback {
		defaultAllocator.Put(buff)
	}
	return
}

func (r *ringbuffer) pop() (p []byte, reuse func()) {
	r.lock.Lock()
	if len(r.buffers) == 0 {
		r.lock.Unlock()
		return
	}
	if buff := r.buffers[r.index]; buff != nil {
		reuse = func() { defaultAllocator.Put(buff) }
		p = buff[r.offset:]
		r.buffers[r.index] = nil
		r.index = (r.index + 1) % len(r.buffers)
		r.offset = 0
	}
	r.lock.Unlock()
	return
}

func (r *ringbuffer) recycle() (n int) {
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
			defaultAllocator.Put(buff)
		}
	}
	r.lock.Unlock()
	return
}

func (r *ringbuffer) hasData() (has bool) {
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
