package http2

import (
	"bufio"
	"bytes"
	"fmt"
	"testing"
)

func TestWriteInt(t *testing.T) {
	n := uint64(15)
	nn := uint64(1337)
	nnn := uint64(122)
	b15 := []byte{15}
	b1337 := []byte{31, 154, 10}
	b122 := []byte{122}
	dst := make([]byte, 3)

	dst = writeInt(dst, 5, n)
	if !bytes.Equal(dst[:1], b15) {
		t.Fatalf("got %v. Expects %v", dst[:1], b15)
	}

	dst = writeInt(dst, 5, nn)
	if !bytes.Equal(dst, b1337) {
		t.Fatalf("got %v. Expects %v", dst, b1337)
	}

	dst[0] = 0
	dst = writeInt(dst, 7, nnn)
	if !bytes.Equal(dst[:1], b122) {
		t.Fatalf("got %v. Expects %v", dst[:1], b122)
	}
}

func TestAppendInt(t *testing.T) {
	n := uint64(15)
	nn := uint64(1337)
	nnn := uint64(122)
	b15 := []byte{15}
	b1337 := []byte{31, 154, 10}
	b122 := []byte{122}
	var dst []byte

	dst = appendInt(dst, 5, n)
	if !bytes.Equal(dst, b15) {
		t.Fatalf("got %v. Expects %v", dst[:1], b15)
	}

	dst = appendInt(dst, 5, nn)
	if !bytes.Equal(dst, b1337) {
		t.Fatalf("got %v. Expects %v", dst, b1337)
	}

	dst[0] = 0
	dst = appendInt(dst[:1], 7, nnn)
	if !bytes.Equal(dst[:1], b122) {
		t.Fatalf("got %v. Expects %v", dst[:1], b122)
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

func TestWriteTwoStrings(t *testing.T) {
	var dstA []byte
	var dstB []byte
	var err error
	strA := []byte(":status")
	strB := []byte("200")

	dst := writeString(nil, strA, false)
	dst = writeString(dst, strB, false)

	dstA, dst, err = readString(nil, dst)
	if err != nil {
		t.Fatal(err)
	}
	dstB, dst, err = readString(nil, dst)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(strA, dstA) {
		t.Fatalf("%s<>%s", dstA, strA)
	}
	if !bytes.Equal(strB, dstB) {
		t.Fatalf("%s<>%s", dstB, strB)
	}
}

func check(t *testing.T, slice []*HeaderField, i int, k, v string) {
	if len(slice) <= i {
		t.Fatalf("fields len exceeded. %d <> %d", len(slice), i)
	}
	hf := slice[i]
	if b2s(hf.name) != k {
		t.Fatalf("unexpected key: %s<>%s", hf.name, k)
	}
	if b2s(hf.value) != v {
		t.Fatalf("unexpected value: %s<>%s", hf.value, v)
	}
}

func TestReadRequestWithoutHuffman(t *testing.T) {
	// TODO:
}

func TestReadRequestWithHuffman(t *testing.T) {
	// TODO:
}

func TestWriteRequestWithoutHuffman(t *testing.T) {
	// TODO:
}

func TestWriteRequestWithHuffman(t *testing.T) {
	// TODO:
}

func TestReadResponseWithoutHuffman(t *testing.T) {
	var err error
	b := []byte{
		0x48, 0x03, 0x33, 0x30, 0x32, 0x58,
		0x07, 0x70, 0x72, 0x69, 0x76, 0x61,
		0x74, 0x65, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x31, 0x20,
		0x47, 0x4d, 0x54, 0x6e, 0x17, 0x68,
		0x74, 0x74, 0x70, 0x73, 0x3a, 0x2f,
		0x2f, 0x77, 0x77, 0x77, 0x2e, 0x65,
		0x78, 0x61, 0x6d, 0x70, 0x6c, 0x65,
		0x2e, 0x63, 0x6f, 0x6d,
	}
	hpack := AcquireHPack()
	hpack.SetMaxTableSize(256)

	b, err = hpack.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	check(t, hpack.fields, 0, ":status", "302")
	check(t, hpack.fields, 1, "cache-control", "private")
	check(t, hpack.fields, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.fields, 3, "location", "https://www.example.com")

	check(t, hpack.dynamic, 0, "location", "https://www.example.com")
	check(t, hpack.dynamic, 1, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 2, "cache-control", "private")
	check(t, hpack.dynamic, 3, ":status", "302")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	b = []byte{0x48, 0x03, 0x33, 0x30, 0x37, 0xc1, 0xc0, 0xbf}
	b, err = hpack.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	check(t, hpack.fields, 0, ":status", "307")
	check(t, hpack.fields, 1, "cache-control", "private")
	check(t, hpack.fields, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.fields, 3, "location", "https://www.example.com")

	check(t, hpack.dynamic, 0, ":status", "307")
	check(t, hpack.dynamic, 1, "location", "https://www.example.com")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 3, "cache-control", "private")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	b = []byte{
		0x88, 0xc1, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x32, 0x20,
		0x47, 0x4d, 0x54, 0xc0, 0x5a, 0x04,
		0x67, 0x7a, 0x69, 0x70, 0x77, 0x38,
		0x66, 0x6f, 0x6f, 0x3d, 0x41, 0x53,
		0x44, 0x4a, 0x4b, 0x48, 0x51, 0x4b,
		0x42, 0x5a, 0x58, 0x4f, 0x51, 0x57,
		0x45, 0x4f, 0x50, 0x49, 0x55, 0x41,
		0x58, 0x51, 0x57, 0x45, 0x4f, 0x49,
		0x55, 0x3b, 0x20, 0x6d, 0x61, 0x78,
		0x2d, 0x61, 0x67, 0x65, 0x3d, 0x33,
		0x36, 0x30, 0x30, 0x3b, 0x20, 0x76,
		0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e,
		0x3d, 0x31,
	}

	b, err = hpack.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	check(t, hpack.fields, 0, ":status", "200")
	check(t, hpack.fields, 1, "cache-control", "private")
	check(t, hpack.fields, 2, "date", "Mon, 21 Oct 2013 20:13:22 GMT")
	check(t, hpack.fields, 3, "location", "https://www.example.com")
	check(t, hpack.fields, 4, "content-encoding", "gzip")
	check(t, hpack.fields, 5, "set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")

	check(t, hpack.dynamic, 0, "set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")
	check(t, hpack.dynamic, 1, "content-encoding", "gzip")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:22 GMT")
	if hpack.tableSize != 215 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 215)
	}

	ReleaseHPack(hpack)
}

func TestReadResponseWithHuffman(t *testing.T) {
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
	hpack.SetMaxTableSize(256)

	b, err = hpack.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	check(t, hpack.fields, 0, ":status", "302")
	check(t, hpack.fields, 1, "cache-control", "private")
	check(t, hpack.fields, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.fields, 3, "location", "https://www.example.com")

	check(t, hpack.dynamic, 0, "location", "https://www.example.com")
	check(t, hpack.dynamic, 1, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 2, "cache-control", "private")
	check(t, hpack.dynamic, 3, ":status", "302")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	b = []byte{0x48, 0x83, 0x64, 0x0e, 0xff, 0xc1, 0xc0, 0xbf}
	b, err = hpack.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	check(t, hpack.fields, 0, ":status", "307")
	check(t, hpack.fields, 1, "cache-control", "private")
	check(t, hpack.fields, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.fields, 3, "location", "https://www.example.com")

	check(t, hpack.dynamic, 0, ":status", "307")
	check(t, hpack.dynamic, 1, "location", "https://www.example.com")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 3, "cache-control", "private")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	b = []byte{
		0x88, 0xc1, 0x61, 0x96, 0xd0, 0x7a,
		0xbe, 0x94, 0x10, 0x54, 0xd4, 0x44,
		0xa8, 0x20, 0x05, 0x95, 0x04, 0x0b,
		0x81, 0x66, 0xe0, 0x84, 0xa6, 0x2d,
		0x1b, 0xff, 0xc0, 0x5a, 0x83, 0x9b,
		0xd9, 0xab, 0x77, 0xad, 0x94, 0xe7,
		0x82, 0x1d, 0xd7, 0xf2, 0xe6, 0xc7,
		0xb3, 0x35, 0xdf, 0xdf, 0xcd, 0x5b,
		0x39, 0x60, 0xd5, 0xaf, 0x27, 0x08,
		0x7f, 0x36, 0x72, 0xc1, 0xab, 0x27,
		0x0f, 0xb5, 0x29, 0x1f, 0x95, 0x87,
		0x31, 0x60, 0x65, 0xc0, 0x03, 0xed,
		0x4e, 0xe5, 0xb1, 0x06, 0x3d, 0x50, 0x07,
	}

	b, err = hpack.Read(b)
	if err != nil {
		t.Fatal(err)
	}

	check(t, hpack.fields, 0, ":status", "200")
	check(t, hpack.fields, 1, "cache-control", "private")
	check(t, hpack.fields, 2, "date", "Mon, 21 Oct 2013 20:13:22 GMT")
	check(t, hpack.fields, 3, "location", "https://www.example.com")
	check(t, hpack.fields, 4, "content-encoding", "gzip")
	check(t, hpack.fields, 5, "set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")

	check(t, hpack.dynamic, 0, "set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")
	check(t, hpack.dynamic, 1, "content-encoding", "gzip")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:22 GMT")
	if hpack.tableSize != 215 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 215)
	}

	ReleaseHPack(hpack)
}

func compare(b, r []byte) int {
	for i, c := range b {
		if c != r[i] {
			return i
		}
	}
	return -1
}

func TestWriteResponseWithoutHuffman(t *testing.T) { // without huffman
	result := []byte{
		0x48, 0x03, 0x33, 0x30, 0x32, 0x58,
		0x07, 0x70, 0x72, 0x69, 0x76, 0x61,
		0x74, 0x65, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x31, 0x20,
		0x47, 0x4d, 0x54, 0x6e, 0x17, 0x68,
		0x74, 0x74, 0x70, 0x73, 0x3a, 0x2f,
		0x2f, 0x77, 0x77, 0x77, 0x2e, 0x65,
		0x78, 0x61, 0x6d, 0x70, 0x6c, 0x65,
		0x2e, 0x63, 0x6f, 0x6d,
	}
	hpack := AcquireHPack()
	hpack.DisableCompression = true
	hpack.SetMaxTableSize(256)

	hpack.Add(":status", "302")
	hpack.Add("cache-control", "private")
	hpack.Add("date", "Mon, 21 Oct 2013 20:13:21 GMT")
	hpack.Add("location", "https://www.example.com")

	b, err := hpack.Write(nil)
	if err != nil {
		t.Fatal(err)
	}
	if i := compare(b, result); i != -1 {
		t.Fatalf("failed in %d: %s", i, hexComparision(b[i:], result[i:]))
	}
	check(t, hpack.dynamic, 0, "location", "https://www.example.com")
	check(t, hpack.dynamic, 1, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 2, "cache-control", "private")
	check(t, hpack.dynamic, 3, ":status", "302")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	result = []byte{0x48, 0x03, 0x33, 0x30, 0x37, 0xc1, 0xc0, 0xbf}
	hpack.Add(":status", "307")
	hpack.Add("cache-control", "private")
	hpack.Add("date", "Mon, 21 Oct 2013 20:13:21 GMT")
	hpack.Add("location", "https://www.example.com")

	b, err = hpack.Write(b[:0])
	if err != nil {
		t.Fatal(err)
	}
	if i := compare(b, result); i != -1 {
		t.Fatalf("failed in %d: %s", i, hexComparision(b[i:], result[i:]))
	}
	check(t, hpack.dynamic, 0, ":status", "307")
	check(t, hpack.dynamic, 1, "location", "https://www.example.com")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 3, "cache-control", "private")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	result = []byte{
		0x88, 0xc1, 0x61, 0x1d, 0x4d, 0x6f,
		0x6e, 0x2c, 0x20, 0x32, 0x31, 0x20,
		0x4f, 0x63, 0x74, 0x20, 0x32, 0x30,
		0x31, 0x33, 0x20, 0x32, 0x30, 0x3a,
		0x31, 0x33, 0x3a, 0x32, 0x32, 0x20,
		0x47, 0x4d, 0x54, 0xc0, 0x5a, 0x04,
		0x67, 0x7a, 0x69, 0x70, 0x77, 0x38,
		0x66, 0x6f, 0x6f, 0x3d, 0x41, 0x53,
		0x44, 0x4a, 0x4b, 0x48, 0x51, 0x4b,
		0x42, 0x5a, 0x58, 0x4f, 0x51, 0x57,
		0x45, 0x4f, 0x50, 0x49, 0x55, 0x41,
		0x58, 0x51, 0x57, 0x45, 0x4f, 0x49,
		0x55, 0x3b, 0x20, 0x6d, 0x61, 0x78,
		0x2d, 0x61, 0x67, 0x65, 0x3d, 0x33,
		0x36, 0x30, 0x30, 0x3b, 0x20, 0x76,
		0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e,
		0x3d, 0x31,
	}

	hpack.Add(":status", "200")
	hpack.Add("cache-control", "private")
	hpack.Add("date", "Mon, 21 Oct 2013 20:13:22 GMT")
	hpack.Add("location", "https://www.example.com")
	hpack.Add("content-encoding", "gzip")
	hpack.Add("set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")

	b, err = hpack.Write(b[:0])
	if err != nil {
		t.Fatal(err)
	}
	if i := compare(b, result); i != -1 {
		t.Fatalf("failed in %d: %s", i, hexComparision(b[i:], result[i:]))
	}

	check(t, hpack.dynamic, 0, "set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")
	check(t, hpack.dynamic, 1, "content-encoding", "gzip")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:22 GMT")
	if hpack.tableSize != 215 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 215)
	}

	ReleaseHPack(hpack)
}

func TestWriteResponseWithHuffman(t *testing.T) { // WithHuffman
	result := []byte{
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
	hpack.SetMaxTableSize(256)
	hpack.Add(":status", "302")
	hpack.Add("cache-control", "private")
	hpack.Add("date", "Mon, 21 Oct 2013 20:13:21 GMT")
	hpack.Add("location", "https://www.example.com")

	b, err := hpack.Write(nil)
	if err != nil {
		t.Fatal(err)
	}
	if i := compare(b, result); i != -1 {
		t.Fatalf("failed in %d: %s", i, hexComparision(b[i:], result[i:]))
	}
	check(t, hpack.dynamic, 0, "location", "https://www.example.com")
	check(t, hpack.dynamic, 1, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 2, "cache-control", "private")
	check(t, hpack.dynamic, 3, ":status", "302")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	result = []byte{0x48, 0x83, 0x64, 0x0e, 0xff, 0xc1, 0xc0, 0xbf}
	hpack.Add(":status", "307")
	hpack.Add("cache-control", "private")
	hpack.Add("date", "Mon, 21 Oct 2013 20:13:21 GMT")
	hpack.Add("location", "https://www.example.com")

	b, err = hpack.Write(b[:0])
	if err != nil {
		t.Fatal(err)
	}
	if i := compare(b, result); i != -1 {
		t.Fatalf("failed in %d: %s", i, hexComparision(b[i:], result[i:]))
	}

	check(t, hpack.dynamic, 0, ":status", "307")
	check(t, hpack.dynamic, 1, "location", "https://www.example.com")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:21 GMT")
	check(t, hpack.dynamic, 3, "cache-control", "private")
	if hpack.tableSize != 222 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 222)
	}

	hpack.releaseFields()

	result = []byte{
		0x88, 0xc1, 0x61, 0x96, 0xd0, 0x7a,
		0xbe, 0x94, 0x10, 0x54, 0xd4, 0x44,
		0xa8, 0x20, 0x05, 0x95, 0x04, 0x0b,
		0x81, 0x66, 0xe0, 0x84, 0xa6, 0x2d,
		0x1b, 0xff, 0xc0, 0x5a, 0x83, 0x9b,
		0xd9, 0xab, 0x77, 0xad, 0x94, 0xe7,
		0x82, 0x1d, 0xd7, 0xf2, 0xe6, 0xc7,
		0xb3, 0x35, 0xdf, 0xdf, 0xcd, 0x5b,
		0x39, 0x60, 0xd5, 0xaf, 0x27, 0x08,
		0x7f, 0x36, 0x72, 0xc1, 0xab, 0x27,
		0x0f, 0xb5, 0x29, 0x1f, 0x95, 0x87,
		0x31, 0x60, 0x65, 0xc0, 0x03, 0xed,
		0x4e, 0xe5, 0xb1, 0x06, 0x3d, 0x50, 0x07,
	}
	hpack.Add(":status", "200")
	hpack.Add("cache-control", "private")
	hpack.Add("date", "Mon, 21 Oct 2013 20:13:22 GMT")
	hpack.Add("location", "https://www.example.com")
	hpack.Add("content-encoding", "gzip")
	hpack.Add("set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")

	b, err = hpack.Write(b[:0])
	if err != nil {
		t.Fatal(err)
	}
	if i := compare(b, result); i != -1 {
		t.Fatalf("failed in %d: %s", i, hexComparision(b[i:], result[i:]))
	}

	check(t, hpack.dynamic, 0, "set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1")
	check(t, hpack.dynamic, 1, "content-encoding", "gzip")
	check(t, hpack.dynamic, 2, "date", "Mon, 21 Oct 2013 20:13:22 GMT")
	if hpack.tableSize != 215 {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.tableSize, 215)
	}

	ReleaseHPack(hpack)
}

func hexComparision(b, r []byte) (s string) {
	for i := range b {
		s += fmt.Sprintf("%x", b[i]) + " "
	}
	s += "\n"
	for i := range r {
		s += fmt.Sprintf("%x", r[i]) + " "
	}
	return
}
