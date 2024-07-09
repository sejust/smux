package smux

import (
	"fmt"
	"sync"
)

func newServerClient() (*Stream, *Stream, func()) {
	c1, c2, err := getTCPConnectionPair()
	if err != nil {
		panic(err)
	}
	s, err := Server(c2, nil, nil)
	if err != nil {
		panic(err)
	}
	c, err := Client(c1, nil, nil)
	if err != nil {
		panic(err)
	}
	var ss *Stream
	done := make(chan error)
	go func() {
		var rerr error
		ss, rerr = s.AcceptStream()
		done <- rerr
		close(done)
	}()
	cs, err := c.OpenStream()
	if err != nil {
		panic(err)
	}
	if err = <-done; err != nil {
		panic(err)
	}
	return ss, cs, func() { c.Close(); s.Close() }
}

func ExampleStream_e1v1() {
	ss, cs, cl := newServerClient()
	defer cl()

	go func() {
		ss.Read(make([]byte, 4))
		ss.Write([]byte("pong"))
	}()

	data := make([]byte, 4)
	n, _ := cs.Write([]byte("ping"))
	cs.Read(data)
	fmt.Println(string(data[:n]))

	// Output:
	// pong
}

func ExampleStream_e3v1() {
	ss, cs, cl := newServerClient()
	defer cl()

	var send int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		data := make([]byte, 1<<16)
		for {
			n, err := ss.Read(data)
			send += n
			if err != nil {
				return
			}
		}
	}()

	data := make([]byte, 1<<16)
	for range [1 << 14]struct{}{} { // 1 GB
		cs.Write(data)
	}
	cs.Close()
	wg.Wait()
	fmt.Println("send:", send)

	// Output:
	// send: 1073741824
}

func ExampleStream_e1v3() {
	ss, cs, cl := newServerClient()
	defer cl()

	go func() {
		data := make([]byte, 1<<16)
		for range [1 << 14]struct{}{} { // 1 GB
			cs.Write(data)
		}
		cs.Close()
	}()

	var received int
	data := make([]byte, 1<<16)
	for {
		n, err := ss.Read(data)
		received += n
		if err != nil {
			break
		}
	}
	fmt.Println("received:", received)

	// Output:
	// received: 1073741824
}

func ExampleStream_e3v3() {
	ss, cs, cl := newServerClient()
	defer cl()

	var send, received int
	go func() {
		data := make([]byte, 1<<15)
		for {
			n, err := cs.Read(data)
			if err != nil {
				return
			}
			cs.Write(data[:n/2])
		}
	}()

	data := make([]byte, 1<<15)
	for range [10]struct{}{} {
		n, _ := ss.Write(data)
		send += n
		n, _ = ss.Read(data)
		received += n
	}
	fmt.Println("send:", send)
	fmt.Println("received:", received)

	// Output:
	// send: 327680
	// received: 163840
}
