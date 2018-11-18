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

func checkField(t *testing.T, hpack *HPack, i int, k, v string) {
	if len(hpack.fields) <= i {
		t.Fatalf("fields len exceeded. %d <> %d", len(hpack.fields), i)
	}
	hf := hpack.fields[i]
	if b2s(hf.name) != k {
		t.Fatalf("unexpected key: %s<>%s", hf.name, k)
	}
	if b2s(hf.value) != v {
		t.Fatalf("unexpected value: %s<>%s", hf.value, v)
	}
}

func TestReadHeaderField(t *testing.T) {
	var err error
	b := []byte{
		0x48, 0x82, 0x64, 0x02, 0x58, 0x85,
		0xae, 0xc3, 0x77, 0x1a, 0x4b, 0x61,
		0x96, 0xd0, 0x7a, 0xbe, 0x94, 0x10,
		0x54, 0xd4, 0x44, 0xa8, 0x20, 0x05,
		0x95, 0x04, 0x0b, 0x81, 0x66, 0xe0,
		0x82, 0xa6, 0x2d, 0x1b, 0xff, 0x6e,
		0x91, 0x9d, 0x29, 0xad, 0x17, 0x18,
		0x63, 0xc7, 0x8f, 0x0b, 0x97, 0xc8,
		0xe9, 0xae, 0x82, 0xae, 0x43, 0xd3,
	}
	hpack := AcquireHPack()

	b, err = hpack.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	checkField(t, hpack, 0, ":status", "302")
	checkField(t, hpack, 1, "cache-control", "private")
	checkField(t, hpack, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	checkField(t, hpack, 3, "location", "https://www.example.com")

	ReleaseHPack(hpack)
}

func TestWriteHeaderField(t *testing.T) {
	// TODO
}
