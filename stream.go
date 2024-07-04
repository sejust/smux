package smux

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Stream implements net.Conn
type Stream struct {
	id   uint32
	sess *Session

	buffer    *ringbuffer
	frameSize int

	// notify a read event
	chReadEvent chan struct{}

	// flag the stream has closed
	die     chan struct{}
	dieOnce sync.Once

	// FIN command
	chFinEvent   chan struct{}
	finEventOnce sync.Once

	// deadlines
	readDeadline  atomic.Value
	writeDeadline atomic.Value

	// per stream sliding window control
	numRead    uint32 // number of consumed bytes
	numWritten uint32 // count num of bytes written
	incr       uint32 // counting for sending

	// UPD command
	peerConsumed uint32        // num of bytes the peer has consumed
	peerWindow   uint32        // peer window, initialized to 256KB, updated by peer
	chUpdate     chan struct{} // notify of remote data consuming and window update
}

// newStream initiates a Stream struct
func newStream(id uint32, frameSize int, sess *Session) *Stream {
	s := new(Stream)
	s.id = id
	s.buffer = newRingbuffer()
	s.chReadEvent = make(chan struct{}, 1)
	s.chUpdate = make(chan struct{}, 1)
	s.frameSize = frameSize
	s.sess = sess
	s.die = make(chan struct{})
	s.chFinEvent = make(chan struct{})
	s.peerWindow = initialPeerWindow // set to initial window size
	return s
}

// ID returns the unique stream ID.
func (s *Stream) ID() uint32 {
	return s.id
}

// Read implements net.Conn
func (s *Stream) Read(b []byte) (n int, err error) {
	for {
		n, err = s.tryRead(b)
		if err == ErrWouldBlock {
			if ew := s.waitRead(); ew != nil {
				return 0, ew
			}
		} else {
			return n, err
		}
	}
}

// tryRead is the nonblocking version of Read
func (s *Stream) tryRead(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	if s.sess.config.Version == 2 {
		return s.tryReadv2(b)
	}

	n = s.buffer.Read(b)
	if n > 0 {
		s.sess.returnTokens(n)
		return n, nil
	}

	select {
	case <-s.die:
		return 0, io.EOF
	default:
		return 0, ErrWouldBlock
	}
}

func (s *Stream) tryReadv2(b []byte) (n int, err error) {
	var notifyConsumed uint32
	n = s.buffer.Read(b)

	// in an ideal environment:
	// if more than half of buffer has consumed, send read ack to peer
	// based on round-trip time of ACK, continous flowing data
	// won't slow down because of waiting for ACK, as long as the
	// consumer keeps on reading data
	// s.numRead == n also notify window at the first read
	s.numRead += uint32(n)
	s.incr += uint32(n)
	if s.incr >= uint32(s.sess.config.MaxStreamBuffer/2) || s.numRead == uint32(n) {
		notifyConsumed = s.numRead
		s.incr = 0
	}

	if n > 0 {
		s.sess.returnTokens(n)
		if notifyConsumed > 0 {
			err := s.sendWindowUpdate(notifyConsumed)
			return n, err
		} else {
			return n, nil
		}
	}

	select {
	case <-s.die:
		return 0, io.EOF
	default:
		return 0, ErrWouldBlock
	}
}

// WriteTo implements io.WriteTo
func (s *Stream) WriteTo(w io.Writer) (n int64, err error) {
	if s.sess.config.Version == 2 {
		return s.writeTov2(w)
	}

	for {
		buf, reuse := s.buffer.Dequeue()
		if buf != nil {
			nw, ew := w.Write(buf)
			s.sess.returnTokens(len(buf))
			reuse()
			if nw > 0 {
				n += int64(nw)
			}

			if ew != nil {
				return n, ew
			}
		} else if ew := s.waitRead(); ew != nil {
			return n, ew
		}
	}
}

func (s *Stream) writeTov2(w io.Writer) (n int64, err error) {
	for {
		var notifyConsumed uint32
		buf, reuse := s.buffer.Dequeue()
		s.numRead += uint32(len(buf))
		s.incr += uint32(len(buf))
		if s.incr >= uint32(s.sess.config.MaxStreamBuffer/2) || s.numRead == uint32(len(buf)) {
			notifyConsumed = s.numRead
			s.incr = 0
		}

		if buf != nil {
			nw, ew := w.Write(buf)
			s.sess.returnTokens(len(buf))
			reuse()
			if nw > 0 {
				n += int64(nw)
			}

			if ew != nil {
				return n, ew
			}

			if notifyConsumed > 0 {
				if err := s.sendWindowUpdate(notifyConsumed); err != nil {
					return n, err
				}
			}
		} else if ew := s.waitRead(); ew != nil {
			return n, ew
		}
	}
}

func (s *Stream) sendWindowUpdate(consumed uint32) error {
	var timer *time.Timer
	var deadline writeDealine
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline.time = d
		deadline.wait = timer.C
	}

	frame := newFrame(byte(s.sess.config.Version), cmdUPD, s.id)
	var hdr updHeader
	binary.LittleEndian.PutUint32(hdr[:], consumed)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(s.sess.config.MaxStreamBuffer))
	frame.data = hdr[:]
	_, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
	return err
}

func (s *Stream) waitRead() error {
	var timer *time.Timer
	var deadline <-chan time.Time
	if d, ok := s.readDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer = time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case <-s.chReadEvent:
		return nil
	case <-s.chFinEvent:
		// BUG(xtaci): Fix for https://github.com/xtaci/smux/issues/82
		if s.buffer.HasData() {
			return nil
		}
		return io.EOF
	case <-s.sess.chSocketReadError:
		return s.sess.socketReadError.Load().(error)
	case <-deadline:
		return ErrTimeout
	case <-s.die:
		return io.ErrClosedPipe
	}
}

// Write implements net.Conn
//
// Note that the behavior when multiple goroutines write concurrently is not deterministic,
// frames may interleave in random way.
func (s *Stream) Write(b []byte) (n int, err error) {
	// check empty input
	if len(b) == 0 {
		return 0, nil
	}
	// check if stream has closed
	select {
	case <-s.die:
		return 0, io.ErrClosedPipe
	default:
	}

	var deadline writeDealine
	if d, ok := s.writeDeadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline.time = d
		deadline.wait = timer.C
	}

	if s.sess.config.Version == 2 {
		return s.writeV2(b, deadline)
	}

	// frame split and transmit
	sent := 0
	frame := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)
	bts := b
	for len(bts) > 0 {
		sz := len(bts)
		if sz > s.frameSize {
			sz = s.frameSize
		}
		frame.data = bts[:sz]
		bts = bts[sz:]
		n, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
		s.numWritten += uint32(sz)
		sent += n
		if err != nil {
			return sent, err
		}
	}

	return sent, nil
}

func (s *Stream) writeV2(b []byte, deadline writeDealine) (n int, err error) {
	// frame split and transmit process
	sent := 0
	frame := newFrame(byte(s.sess.config.Version), cmdPSH, s.id)

	for {
		// per stream sliding window control
		// [.... [consumed... numWritten] ... win... ]
		// [.... [consumed...................+rmtwnd]]
		var bts []byte
		// note:
		// even if uint32 overflow, this math still works:
		// eg1: uint32(0) - uint32(math.MaxUint32) = 1
		// eg2: int32(uint32(0) - uint32(1)) = -1
		// security check for misbehavior
		inflight := int32(s.numWritten - atomic.LoadUint32(&s.peerConsumed))
		if inflight < 0 {
			return 0, ErrConsumed
		}

		win := int32(atomic.LoadUint32(&s.peerWindow)) - inflight
		if win > 0 {
			if win > int32(len(b)) {
				bts = b
				b = nil
			} else {
				bts = b[:win]
				b = b[win:]
			}

			for len(bts) > 0 {
				sz := len(bts)
				if sz > s.frameSize {
					sz = s.frameSize
				}
				frame.data = bts[:sz]
				bts = bts[sz:]
				n, err := s.sess.writeFrameInternal(frame, deadline, CLSDATA)
				s.numWritten += uint32(sz)
				sent += n
				if err != nil {
					return sent, err
				}
			}
		}

		// if there is any data remaining to be sent
		// wait until stream closes, window changes or deadline reached
		// this blocking behavior will inform upper layer to do flow control
		if len(b) > 0 {
			select {
			case <-s.chFinEvent: // if fin arrived, future window update is impossible
				return 0, io.EOF
			case <-s.die:
				return sent, io.ErrClosedPipe
			case <-deadline.wait:
				return sent, ErrTimeout
			case <-s.sess.chSocketWriteError:
				return sent, s.sess.socketWriteError.Load().(error)
			case <-s.chUpdate:
				continue
			}
		} else {
			return sent, nil
		}
	}
}

// Close implements net.Conn
func (s *Stream) Close() error {
	var once bool
	var err error
	s.dieOnce.Do(func() {
		close(s.die)
		once = true
	})

	if once {
		_, err = s.sess.writeFrame(newFrame(byte(s.sess.config.Version), cmdFIN, s.id))
		s.sess.streamClosed(s.id)
		return err
	} else {
		return io.ErrClosedPipe
	}
}

// CloseChan returns a readonly chan which can be readable
// when the stream is to be closed.
func (s *Stream) CloseChan() <-chan struct{} {
	return s.die
}

// IsClosed does a safe check to see if we have shutdown.
func (s *Stream) IsClosed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}

// SetReadDeadline sets the read deadline as defined by
// net.Conn.SetReadDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.readDeadline.Store(t)
	s.notifyReadEvent()
	return nil
}

// SetWriteDeadline sets the write deadline as defined by
// net.Conn.SetWriteDeadline.
// A zero time value disables the deadline.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.writeDeadline.Store(t)
	return nil
}

// SetDeadline sets both read and write deadlines as defined by
// net.Conn.SetDeadline.
// A zero time value disables the deadlines.
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	if err := s.SetWriteDeadline(t); err != nil {
		return err
	}
	return nil
}

// session closes
func (s *Stream) sessionClose() { s.dieOnce.Do(func() { close(s.die) }) }

// LocalAddr satisfies net.Conn interface
func (s *Stream) LocalAddr() net.Addr {
	return s.sess.LocalAddr()
}

// RemoteAddr satisfies net.Conn interface
func (s *Stream) RemoteAddr() net.Addr {
	return s.sess.RemoteAddr()
}

// pushBytes append buf to buffers
func (s *Stream) pushBytes(buf []byte) (written int, err error) {
	s.buffer.Enqueue(buf)
	return
}

// recycleTokens transform remaining bytes to tokens(will truncate buffer)
func (s *Stream) recycleTokens() (n int) {
	return s.buffer.Recycle()
}

// notify read event
func (s *Stream) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}

// update command
func (s *Stream) update(consumed uint32, window uint32) {
	atomic.StoreUint32(&s.peerConsumed, consumed)
	atomic.StoreUint32(&s.peerWindow, window)
	select {
	case s.chUpdate <- struct{}{}:
	default:
	}
}

// mark this stream has been closed in protocol
func (s *Stream) fin() {
	s.finEventOnce.Do(func() {
		close(s.chFinEvent)
	})
}
