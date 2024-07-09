package smux

import (
	"math/rand"
	"testing"
)

func TestAllocGet(t *testing.T) {
	alloc := NewAllocator()
	if alloc.Registered() {
		t.Fatal("registered")
	}
	if _, err := alloc.Get(0); err != ErrAllocOversize {
		t.Fatal(0)
	}
	if b, _ := alloc.Get(1); len(b) != 1 {
		t.Fatal(1)
	}
	if b, _ := alloc.Get(2); len(b) != 2 {
		t.Fatal(2)
	}
	if b, _ := alloc.Get(3); len(b) != 3 || cap(b) != 4 {
		t.Fatal(3)
	}
	if b, _ := alloc.Get(4); len(b) != 4 {
		t.Fatal(4)
	}
	if b, _ := alloc.Get(1023); len(b) != 1023 || cap(b) != 1024 {
		t.Fatal(1023)
	}
	if b, _ := alloc.Get(maxsize); len(b) != maxsize {
		t.Fatal(maxsize)
	}
	if _, err := alloc.Get(maxsize + 1); err != ErrAllocOversize {
		t.Fatal(maxsize + 1)
	}
}

func TestAllocPut(t *testing.T) {
	alloc := NewAllocator()
	if err := alloc.Put(nil); err == nil {
		t.Fatal("put nil misbehavior")
	}
	if err := alloc.Put(make([]byte, 3)); err == nil {
		t.Fatal("put elem:3 []bytes misbehavior")
	}
	if err := alloc.Put(make([]byte, 4)); err != nil {
		t.Fatal("put elem:4 []bytes misbehavior")
	}
	if err := alloc.Put(make([]byte, 1023, 1024)); err != nil {
		t.Fatal("put elem:1024 []bytes misbehavior")
	}
	if err := alloc.Put(make([]byte, 65536)); err != nil {
		t.Fatal("put elem:65536 []bytes misbehavior")
	}
	if err := alloc.Put(make([]byte, 65537)); err == nil {
		t.Fatal("put elem:65537 []bytes misbehavior")
	}
}

func TestAllocPutThenGet(t *testing.T) {
	alloc := NewAllocator()
	data, _ := alloc.Get(4)
	alloc.Put(data)
	newData, _ := alloc.Get(4)
	if cap(data) != cap(newData) {
		t.Fatal("different cap while alloc.Get()")
	}
}

func BenchmarkMSB(b *testing.B) {
	for i := 0; i < b.N; i++ {
		msb(rand.Int())
	}
}
