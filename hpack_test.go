package http2

import (
	"bytes"
	"testing"
)

func TestWriteInt(t *testing.T) {
	n := uint64(15)
	nn := uint64(1337)
	nnn := uint64(122)
	b15 := []byte{15}
	b1337 := []byte{31, 154, 10}
	b122 := []byte{122}
	dst := make([]byte, 1)

	dst = writeInt(dst, 5, n)
	if !bytes.Equal(dst[:1], b15) {
		t.Fatalf("got %v. Expects %v", dst, b15)
	}

	dst = writeInt(dst, 5, nn)
	if !bytes.Equal(dst[:3], b1337) {
		t.Fatalf("got %v. Expects %v", dst, b1337)
	}

	dst = writeInt(dst, 7, nnn)
	if !bytes.Equal(dst[:1], b122) {
		t.Fatalf("got %v. Expects %v", dst, b122)
	}
}

func TestReadInt(t *testing.T) {
	var err error
	n := uint64(0)
	b := []byte{15, 31, 154, 10, 122}

	b, n, err = readInt(5, b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 15 {
		t.Fatalf("%d <> 15", n)
	}
	if len(b) != 4 {
		t.Fatalf("bad length. Got %d. Expected 4", len(b))
	}

	b, n, err = readInt(5, b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1337 {
		t.Fatalf("%d <> 1337", n)
	}
	if len(b) != 1 {
		t.Fatalf("bad length. Got %d. Expected 2", len(b))
	}

	b, n, err = readInt(7, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Fatalf("bad length. Got %d. Expected 0", len(b))
	}
}

func TestReadHeaderField(t *testing.T) {
	// TODO
}

func TestWriteHeaderField(t *testing.T) {
	// TODO
}
