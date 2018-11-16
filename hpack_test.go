package http2

import (
	"bytes"
	"testing"
)

func TestWriteInt(t *testing.T) {
	n := uint64(15)
	nn := uint64(1337)
	b15 := []byte{15}
	b1337 := []byte{31, 154, 10}
	dst := make([]byte, 1)

	dst = writeInt(dst, 5, n)
	if !bytes.Equal(dst[:1], b15) {
		t.Fatalf("got %v. Expects %v", dst, b15)
	}
	dst = writeInt(dst, 5, nn)
	if !bytes.Equal(dst[:3], b1337) {
		t.Fatalf("got %v. Expects %v", dst, b1337)
	}
}

func TestReadInt(t *testing.T) {
	var err error
	n := uint64(0)
	b := []byte{15}

	b, n, err = readInt(5, b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 15 {
		t.Fatalf("%d <> 15", n)
	}

	b = []byte{31, 154, 10}
	b, n, err = readInt(5, b)
	if err != nil {
		t.Fatal(err)
	}

	if n != 1337 {
		t.Fatalf("%d <> 1337", n)
	}
}

func TestReadString(t *testing.T) {
	// TODO:
}

func TestWriteString(t *testing.T) {
	// TODO:
}

func TestReadHeaderField(t *testing.T) {
	// TODO
}

func TestWriteHeaderField(t *testing.T) {
	// TODO
}
