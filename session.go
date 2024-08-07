package smux

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAcceptBacklog = 1024
	openCloseTimeout     = 30 * time.Second // stream open/close timeout
)

// define frame class
type CLASSID uint8

const (
	CLSCTRL CLASSID = iota
	CLSDATA
)

var (
	ErrInvalidProtocol = errors.New("invalid protocol")
	ErrConsumed        = errors.New("peer consumed more than sent")
	ErrGoAway          = errors.New("stream id overflows, should start a new connection")
	ErrTimeout         = errors.New("timeout")
	ErrWouldBlock      = errors.New("operation would block on IO")
	ErrAllocOversize   = errors.New("allocator was oversize")
)

type writeRequest struct {
	frame    Frame
	deadline time.Time
	result   chan writeResult
}

type writeResult struct {
	n   int
	err error
}

type writeDealine struct {
	time time.Time
	wait <-chan time.Time
}

// Session defines a multiplexed connection for streams
type Session struct {
	conn io.ReadWriteCloser

	config           *Config
	nextStreamID     uint32 // next stream identifier
	nextStreamIDLock sync.Mutex

	bucket       int32         // token bucket
	bucketNotify chan struct{} // used for waiting for tokens

	streams    map[uint32]*Stream // all streams in this session
	streamLock sync.RWMutex       // locks streams

	die     chan struct{} // flag session has died
	dieOnce sync.Once

	// socket error handling
	socketReadError      atomic.Value
	socketWriteError     atomic.Value
	chSocketReadError    chan struct{}
	chSocketWriteError   chan struct{}
	socketReadErrorOnce  sync.Once
	socketWriteErrorOnce sync.Once

	chAccepts chan *Stream

	dataReady int32 // flag data has arrived

	goAway int32 // flag id exhausted

	deadline atomic.Value

	ctrl         chan writeRequest // a ctrl frame for writing
	writes       chan writeRequest
	resultChPool sync.Pool

	allocator Allocator
}

func newSession(config *Config, conn io.ReadWriteCloser, allocator Allocator, client bool) *Session {
	s := new(Session)
	s.die = make(chan struct{})
	s.conn = conn
	s.config = config
	s.streams = make(map[uint32]*Stream)
	s.chAccepts = make(chan *Stream, defaultAcceptBacklog)
	s.bucket = int32(config.MaxReceiveBuffer)
	s.bucketNotify = make(chan struct{}, 1)
	s.ctrl = make(chan writeRequest, 1)
	s.writes = make(chan writeRequest)
	s.resultChPool = sync.Pool{New: func() interface{} {
		return make(chan writeResult, 1)
	}}
	s.chSocketReadError = make(chan struct{})
	s.chSocketWriteError = make(chan struct{})
	s.allocator = allocator

	if client {
		s.nextStreamID = 1
	} else {
		s.nextStreamID = 0
	}

	go s.recvLoop()
	go s.sendLoop()
	if !config.KeepAliveDisabled {
		go s.keepalive(client)
	}
	return s
}

// OpenStream is used to create a new stream
func (s *Session) OpenStream() (*Stream, error) {
	if s.IsClosed() {
		return nil, io.ErrClosedPipe
	}

	// generate stream id
	s.nextStreamIDLock.Lock()
	if s.goAway > 0 {
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}

	s.nextStreamID += 2
	sid := s.nextStreamID
	if sid == sid%2 { // stream-id overflows
		s.goAway = 1
		s.nextStreamIDLock.Unlock()
		return nil, ErrGoAway
	}
	s.nextStreamIDLock.Unlock()

	stream := newStream(sid, s.config.MaxFrameSize, s)

	if _, err := s.writeFrame(newFrame(byte(s.config.Version), cmdSYN, sid)); err != nil {
		return nil, err
	}

	select {
	case <-s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <-s.chSocketWriteError:
		return nil, s.socketWriteError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	default:
		s.streamLock.Lock()
		s.streams[sid] = stream
		s.streamLock.Unlock()
		return stream, nil
	}
}

// Open returns a generic ReadWriteCloser
func (s *Session) Open() (io.ReadWriteCloser, error) {
	return s.OpenStream()
}

// AcceptStream is used to block until the next available stream
// is ready to be accepted.
func (s *Session) AcceptStream() (*Stream, error) {
	var deadline <-chan time.Time
	if d, ok := s.deadline.Load().(time.Time); ok && !d.IsZero() {
		timer := time.NewTimer(time.Until(d))
		defer timer.Stop()
		deadline = timer.C
	}

	select {
	case stream := <-s.chAccepts:
		return stream, nil
	case <-deadline:
		return nil, ErrTimeout
	case <-s.chSocketReadError:
		return nil, s.socketReadError.Load().(error)
	case <-s.die:
		return nil, io.ErrClosedPipe
	}
}

// Accept Returns a generic ReadWriteCloser instead of smux.Stream
func (s *Session) Accept() (io.ReadWriteCloser, error) {
	return s.AcceptStream()
}

// Close is used to close the session and all streams.
func (s *Session) Close() error {
	var once bool
	s.dieOnce.Do(func() {
		close(s.die)
		once = true
	})

	if once {
		s.streamLock.Lock()
		for k := range s.streams {
			s.streams[k].sessionClose()
		}
		s.streamLock.Unlock()
		return s.conn.Close()
	} else {
		return io.ErrClosedPipe
	}
}

// CloseChan can be used by someone who wants to be notified immediately when this
// session is closed
func (s *Session) CloseChan() <-chan struct{} {
	return s.die
}

// notifyBucket notifies recvLoop that bucket is available
func (s *Session) notifyBucket() {
	select {
	case s.bucketNotify <- struct{}{}:
	default:
	}
}

func (s *Session) notifyReadError(err error) {
	s.socketReadErrorOnce.Do(func() {
		s.socketReadError.Store(err)
		close(s.chSocketReadError)
	})
}

func (s *Session) notifyWriteError(err error) {
	s.socketWriteErrorOnce.Do(func() {
		s.socketWriteError.Store(err)
		close(s.chSocketWriteError)
	})
}

// IsClosed does a safe check to see if we have shutdown
func (s *Session) IsClosed() bool {
	select {
	case <-s.die:
		return true
	default:
		return false
	}
}

// NumStreams returns the number of currently open streams
func (s *Session) NumStreams() int {
	if s.IsClosed() {
		return 0
	}
	s.streamLock.Lock()
	defer s.streamLock.Unlock()
	return len(s.streams)
}

// SetDeadline sets a deadline used by Accept* calls.
// A zero time value disables the deadline.
func (s *Session) SetDeadline(t time.Time) error {
	s.deadline.Store(t)
	return nil
}

// LocalAddr satisfies net.Conn interface
func (s *Session) LocalAddr() net.Addr {
	if ts, ok := s.conn.(interface {
		LocalAddr() net.Addr
	}); ok {
		return ts.LocalAddr()
	}
	return nil
}

// RemoteAddr satisfies net.Conn interface
func (s *Session) RemoteAddr() net.Addr {
	if ts, ok := s.conn.(interface {
		RemoteAddr() net.Addr
	}); ok {
		return ts.RemoteAddr()
	}
	return nil
}

// notify the session that a stream has closed
func (s *Session) streamClosed(sid uint32) {
	s.streamLock.Lock()
	if n := s.streams[sid].recycleTokens(); n > 0 { // return remaining tokens to the bucket
		if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
			s.notifyBucket()
		}
	}
	delete(s.streams, sid)
	s.streamLock.Unlock()
}

// returnTokens is called by stream to return token after read
func (s *Session) returnTokens(n int) {
	if atomic.AddInt32(&s.bucket, int32(n)) > 0 {
		s.notifyBucket()
	}
}

// recvLoop keeps on reading from underlying connection if tokens are available
func (s *Session) recvLoop() {
	var (
		err error

		hdr     rawHeader
		hdrBuf  = hdr[:]
		hdrCopy = func() {}

		upd     updHeader
		updBuf  = upd[:]
		updCopy = func() {}
	)

	if s.allocator.Registered() {
		hdrBuf, err = s.allocator.Get(len(hdr))
		if err != nil {
			s.notifyReadError(err)
			return
		}
		defer func() { s.allocator.Put(hdrBuf) }()

		updBuf, err = s.allocator.Get(len(upd))
		if err != nil {
			s.notifyReadError(err)
			return
		}
		defer func() { s.allocator.Put(updBuf) }()

		hdrCopy = func() { copy(hdr[:], hdrBuf) }
		updCopy = func() { copy(upd[:], updBuf) }
	}

	for {
		for atomic.LoadInt32(&s.bucket) <= 0 && !s.IsClosed() {
			select {
			case <-s.bucketNotify:
			case <-s.die:
				return
			}
		}

		// read header first
		if _, err := io.ReadFull(s.conn, hdrBuf); err == nil {
			hdrCopy()
			atomic.StoreInt32(&s.dataReady, 1)
			if hdr.Version() != byte(s.config.Version) {
				s.notifyReadError(ErrInvalidProtocol)
				return
			}
			sid := hdr.StreamID()
			switch hdr.Cmd() {
			case cmdNOP:
			case cmdSYN:
				s.streamLock.Lock()
				if _, ok := s.streams[sid]; !ok {
					stream := newStream(sid, s.config.MaxFrameSize, s)
					s.streams[sid] = stream
					select {
					case s.chAccepts <- stream:
					case <-s.die:
					}
				}
				s.streamLock.Unlock()
			case cmdFIN:
				s.streamLock.RLock()
				if stream, ok := s.streams[sid]; ok {
					stream.fin()
					stream.notifyReadEvent()
				}
				s.streamLock.RUnlock()
			case cmdPSH:
				if hdr.Length() > 0 {
					newbuf, err := s.allocator.Get(int(hdr.Length()))
					if err != nil {
						s.notifyReadError(err)
						return
					}
					if written, err := io.ReadFull(s.conn, newbuf); err == nil {
						s.streamLock.RLock()
						if stream, ok := s.streams[sid]; ok {
							stream.pushBytes(newbuf)
							atomic.AddInt32(&s.bucket, -int32(written))
							stream.notifyReadEvent()
						} else {
							s.allocator.Put(newbuf)
						}
						s.streamLock.RUnlock()
					} else {
						s.allocator.Put(newbuf)
						s.notifyReadError(err)
						return
					}
				}
			case cmdUPD:
				if _, err := io.ReadFull(s.conn, updBuf); err == nil {
					updCopy()
					s.streamLock.Lock()
					if stream, ok := s.streams[sid]; ok {
						stream.update(upd.Consumed(), upd.Window())
					}
					s.streamLock.Unlock()
				} else {
					s.notifyReadError(err)
					return
				}
			default:
				s.notifyReadError(ErrInvalidProtocol)
				return
			}
		} else {
			s.notifyReadError(err)
			return
		}
	}
}

func (s *Session) keepalive(client bool) {
	tickerTimeout := time.NewTicker(s.config.KeepAliveTimeout)
	defer tickerTimeout.Stop()
	if !client { // server side
		for {
			select {
			case <-tickerTimeout.C:
				if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
					// recvLoop may block while bucket is 0, in this case,
					// session should not be closed.
					if atomic.LoadInt32(&s.bucket) > 0 {
						s.Close()
						return
					}
				}
			case <-s.die:
				return
			}
		}
	}

	tickerPing := time.NewTicker(s.config.KeepAliveInterval)
	defer tickerPing.Stop()
	alive := false
	for {
		select {
		case <-tickerPing.C:
			deadline := writeDealine{
				time: time.Now().Add(s.config.KeepAliveInterval),
				wait: tickerPing.C,
			}
			_, err := s.writeFrameInternal(newFrame(byte(s.config.Version), cmdNOP, 0), deadline, CLSCTRL)
			if err == nil {
				alive = true
			}
			s.notifyBucket() // force a signal to the recvLoop
		case <-tickerTimeout.C:
			if !atomic.CompareAndSwapInt32(&s.dataReady, 1, 0) {
				// recvLoop may block while bucket is 0, in this case,
				// session should not be closed.
				if atomic.LoadInt32(&s.bucket) > 0 && !alive {
					s.Close()
					return
				}
			}
			alive = false
		case <-s.die:
			return
		}
	}
}

func (s *Session) sendLoop() {
	var buf []byte
	var n int
	var err error
	var vec [][]byte // vector for writeBuffers
	var request writeRequest

	bw, ok := s.conn.(interface {
		WriteBuffers(v [][]byte) (n int, err error)
	})

	if ok {
		if s.allocator.Registered() {
			buf, err = s.allocator.Get(headerSize)
		} else {
			buf = make([]byte, headerSize)
		}
		vec = make([][]byte, 2)
	} else {
		size := s.config.MaxFrameSize + headerSize
		if s.allocator.Registered() {
			buf, err = s.allocator.Get(size)
		} else {
			buf = make([]byte, size)
		}
	}
	if err != nil {
		s.notifyWriteError(err)
		return
	}
	if s.allocator.Registered() {
		defer func() { s.allocator.Put(buf) }()
	}

	setWriteDeadline := func(t time.Time) error { return nil }
	if wd, ok := s.conn.(interface {
		SetWriteDeadline(t time.Time) error
	}); ok {
		setWriteDeadline = wd.SetWriteDeadline
	}

	for {
		select {
		case request = <-s.ctrl:
		default:
			select {
			case <-s.die:
				return
			case request = <-s.ctrl:
			case request = <-s.writes:
			}
		}

		{
			buf[0] = (request.frame.ver << 4) | (request.frame.cmd & 0x0f)
			binary.LittleEndian.PutUint32(buf[1:], uint32(len(request.frame.data)))
			binary.LittleEndian.PutUint32(buf[4:], request.frame.sid)

			setWriteDeadline(request.deadline)
			if len(vec) > 0 {
				vec[0] = buf[:headerSize]
				vec[1] = request.frame.data
				n, err = bw.WriteBuffers(vec)
			} else {
				copy(buf[headerSize:], request.frame.data)
				n, err = s.conn.Write(buf[:headerSize+len(request.frame.data)])
			}

			n -= headerSize
			if n < 0 {
				n = 0
			}

			result := writeResult{
				n:   n,
				err: err,
			}

			request.result <- result

			// store conn error
			if err != nil {
				s.notifyWriteError(err)
				return
			}
		}
	}
}

// writeFrame writes the frame to the underlying connection
// and returns the number of bytes written if successful
func (s *Session) writeFrame(f Frame) (n int, err error) {
	timer := time.NewTimer(openCloseTimeout)
	defer timer.Stop()
	deadline := writeDealine{
		time: time.Now().Add(openCloseTimeout),
		wait: timer.C,
	}
	return s.writeFrameInternal(f, deadline, CLSCTRL)
}

// internal writeFrame version to support deadline used in keepalive
func (s *Session) writeFrameInternal(f Frame, deadline writeDealine, class CLASSID) (int, error) {
	req := writeRequest{
		frame:    f,
		deadline: deadline.time,
		result:   s.resultChPool.Get().(chan writeResult),
	}
	writeCh := s.writes
	if class == CLSCTRL {
		writeCh = s.ctrl
	}

	select {
	case <-deadline.wait:
		return 0, ErrTimeout
	default:
		select {
		case writeCh <- req:
		case <-s.die:
			return 0, io.ErrClosedPipe
		case <-s.chSocketWriteError:
			return 0, s.socketWriteError.Load().(error)
		case <-deadline.wait:
			return 0, ErrTimeout
		}
	}

	select {
	case result := <-req.result:
		s.resultChPool.Put(req.result)
		return result.n, result.err
	case <-s.die:
		return 0, io.ErrClosedPipe
	case <-s.chSocketWriteError:
		return 0, s.socketWriteError.Load().(error)
	}
}
