package http2

import (
	"bufio"
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

func checkInt(t *testing.T, err error, n, e uint64, elen int, b []byte) {
	if err != nil {
		t.Fatal(err)
	}
	if n != e {
		t.Fatalf("%d <> %d", n, e)
	}
	if b != nil && len(b) != elen {
		t.Fatalf("bad length. Got %d. Expected %d", len(b), elen)
	}
}

func TestReadInt(t *testing.T) {
	var err error
	n := uint64(0)
	b := []byte{15, 31, 154, 10, 122}

	b, n, err = readInt(5, b)
	checkInt(t, err, n, 15, 4, b)

	b, n, err = readInt(5, b)
	checkInt(t, err, n, 1337, 1, b)

	b, n, err = readInt(7, b)
	checkInt(t, err, n, 122, 0, b)
}

func TestReadIntFrom(t *testing.T) {
	var n uint64
	var err error
	br := bufio.NewReader(
		bytes.NewBuffer([]byte{15, 31, 154, 10, 122}),
	)

	n, err = readIntFrom(7, br)
	checkInt(t, err, n, 15, 0, nil)

	n, err = readIntFrom(5, br)
	checkInt(t, err, n, 1337, 0, nil)

	n, err = readIntFrom(7, br)
	checkInt(t, err, n, 122, 0, nil)
}

func TestReadHeaderField(t *testing.T) {
	// TODO
}

func TestWriteHeaderField(t *testing.T) {
	// TODO
}
