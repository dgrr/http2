package http2

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestHPACKAppendInt(t *testing.T) {
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
	t.Helper()

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

func TestHPACKReadInt(t *testing.T) {
	var err error
	var n uint64
	b := []byte{15, 31, 154, 10, 122}

	b, n = readInt(5, b)
	checkInt(t, err, n, 15, 4, b)

	b, n = readInt(5, b)
	checkInt(t, err, n, 1337, 1, b)

	b, n = readInt(7, b)
	checkInt(t, err, n, 122, 0, b)
}

func TestHPACKWriteTwoStrings(t *testing.T) {
	var dstA []byte
	var dstB []byte
	var err error

	strA := []byte(":status")
	strB := []byte("200")

	dst := appendString(nil, strA, false)
	dst = appendString(dst, strB, false)

	dst, dstA, err = readString(nil, dst)
	if err != nil {
		t.Fatal(err)
	}

	_, dstB, err = readString(nil, dst)
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
	t.Helper()

	if len(slice) <= i {
		t.Fatalf("fields len exceeded. %d <> %d", len(slice), i)
	}

	hf := slice[i]
	if string(hf.key) != k {
		t.Fatalf("unexpected key: %s<>%s", hf.key, k)
	}

	if string(hf.value) != v {
		t.Fatalf("unexpected value: %s<>%s", hf.value, v)
	}
}

func readHPACKAndCheck(t *testing.T, hpack *HPACK, b []byte, fields, table []string, tableSize int) {
	t.Helper()

	var err error
	var lck sync.Mutex
	var ok bool

	go func() {
		// timeout in case a header has any error
		time.Sleep(time.Second * 2)
		lck.Lock()
		ok = true
		lck.Unlock()
	}()

	hfields := make([]*HeaderField, len(fields)/2)
	for i := 0; len(b) > 0 && !ok; i++ {
		hfields[i] = AcquireHeaderField()
		b, err = hpack.Next(hfields[i], b)
		if err != nil {
			t.Fatal(err)
		}
	}
	lck.Lock()
	ok = true
	lck.Unlock()

	if len(b) > 0 {
		t.Fatal("error reading headers: timeout")
	}

	n := 0
	for i := 0; i < len(fields); i += 2 {
		check(t, hfields, n, fields[i], fields[i+1])
		n++
	}
	n = 0
	for i := len(table) - 1; i >= 0; i -= 2 {
		check(t, hpack.dynamic, n, table[i-1], table[i])
		n++
	}

	if hpack.DynamicSize() != tableSize {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.DynamicSize(), tableSize)
	}
}

func TestHPACKReadRequestWithoutHuffman(t *testing.T) {
	b := []byte{
		0x82, 0x86, 0x84, 0x41, 0x0f, 0x77,
		0x77, 0x77, 0x2e, 0x65, 0x78, 0x61,
		0x6d, 0x70, 0x6c, 0x65, 0x2e, 0x63,
		0x6f, 0x6d,
	}
	hpack := AcquireHPACK()

	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	b = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x08,
		0x6e, 0x6f, 0x2d, 0x63, 0x61, 0x63,
		0x68, 0x65,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	b = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x0a,
		0x63, 0x75, 0x73, 0x74, 0x6f, 0x6d,
		0x2d, 0x6b, 0x65, 0x79, 0x0c, 0x63,
		0x75, 0x73, 0x74, 0x6f, 0x6d, 0x2d,
		0x76, 0x61, 0x6c, 0x75, 0x65,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)
	ReleaseHPACK(hpack)
}

func TestHPACKReadRequestWithHuffman(t *testing.T) {
	b := []byte{
		0x82, 0x86, 0x84, 0x41, 0x8c, 0xf1,
		0xe3, 0xc2, 0xe5, 0xf2, 0x3a, 0x6b,
		0xa0, 0xab, 0x90, 0xf4, 0xff,
	}
	hpack := AcquireHPACK()

	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	b = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x86,
		0xa8, 0xeb, 0x10, 0x64, 0x9c, 0xbf,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	b = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x88,
		0x25, 0xa8, 0x49, 0xe9, 0x5b, 0xa9,
		0x7d, 0x7f, 0x89, 0x25, 0xa8, 0x49,
		0xe9, 0x5b, 0xb8, 0xe8, 0xb4, 0xbf,
	}
	readHPACKAndCheck(t, hpack, b, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)
	ReleaseHPACK(hpack)
}

func TestHPACKReadResponseWithoutHuffman(t *testing.T) {
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
	hpack := AcquireHPACK()
	hpack.SetMaxTableSize(256)

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	b = []byte{0x48, 0x03, 0x33, 0x30, 0x37, 0xc1, 0xc0, 0xbf}

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

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

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func TestHPACKReadResponseWithHuffman(t *testing.T) {
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
	hpack := AcquireHPACK()
	hpack.SetMaxTableSize(256)

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	b = []byte{0x48, 0x83, 0x64, 0x0e, 0xff, 0xc1, 0xc0, 0xbf}

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

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

	readHPACKAndCheck(t, hpack, b, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func compare(b, r []byte) int {
	for i, c := range b {
		if c != r[i] {
			return i
		}
	}
	return -1
}

func writeHPACKAndCheck(t *testing.T, hpack *HPACK, r []byte, fields, table []string, tableSize int) {
	t.Helper()

	n := 0
	hfs := make([]*HeaderField, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		hf := AcquireHeaderField()
		hf.Set(fields[i], fields[i+1])
		hfs = append(hfs, hf)
		n++
	}

	var b []byte

	for _, hf := range hfs {
		b = hpack.AppendHeader(b, hf, true)
	}

	if i := compare(b, r); i != -1 {
		t.Fatalf("failed in %d (%d): %s", i, tableSize, hexComparision(b[i:], r[i:]))
	}

	n = 0
	for i := len(table) - 1; i >= 0; i -= 2 {
		check(t, hpack.dynamic, n, table[i-1], table[i])
		n++
	}

	if hpack.DynamicSize() != tableSize {
		t.Fatalf("Unexpected table size: %d<>%d", hpack.DynamicSize(), tableSize)
	}
}

func TestHPACKWriteRequestWithoutHuffman(t *testing.T) {
	r := []byte{
		0x82, 0x86, 0x84, 0x41, 0x0f, 0x77,
		0x77, 0x77, 0x2e, 0x65, 0x78, 0x61,
		0x6d, 0x70, 0x6c, 0x65, 0x2e, 0x63,
		0x6f, 0x6d,
	}
	hpack := AcquireHPACK()
	hpack.DisableCompression = true

	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	r = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x08,
		0x6e, 0x6f, 0x2d, 0x63, 0x61, 0x63,
		0x68, 0x65,
	}
	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	r = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x0a,
		0x63, 0x75, 0x73, 0x74, 0x6f, 0x6d,
		0x2d, 0x6b, 0x65, 0x79, 0x0c, 0x63,
		0x75, 0x73, 0x74, 0x6f, 0x6d, 0x2d,
		0x76, 0x61, 0x6c, 0x75, 0x65,
	}

	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)

	ReleaseHPACK(hpack)
}

func TestHPACKWriteRequestWithHuffman(t *testing.T) {
	r := []byte{
		0x82, 0x86, 0x84, 0x41, 0x8c, 0xf1,
		0xe3, 0xc2, 0xe5, 0xf2, 0x3a, 0x6b,
		0xa0, 0xab, 0x90, 0xf4, 0xff,
	}
	hpack := AcquireHPACK()

	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
	}, []string{
		":authority", "www.example.com",
	}, 57)

	r = []byte{
		0x82, 0x86, 0x84, 0xbe, 0x58, 0x86,
		0xa8, 0xeb, 0x10, 0x64, 0x9c, 0xbf,
	}
	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "http",
		":path", "/",
		":authority", "www.example.com",
		"cache-control", "no-cache",
	}, []string{
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 110)

	r = []byte{
		0x82, 0x87, 0x85, 0xbf, 0x40, 0x88,
		0x25, 0xa8, 0x49, 0xe9, 0x5b, 0xa9,
		0x7d, 0x7f, 0x89, 0x25, 0xa8, 0x49,
		0xe9, 0x5b, 0xb8, 0xe8, 0xb4, 0xbf,
	}
	writeHPACKAndCheck(t, hpack, r, []string{
		":method", "GET",
		":scheme", "https",
		":path", "/index.html",
		":authority", "www.example.com",
		"custom-key", "custom-value",
	}, []string{
		"custom-key", "custom-value",
		"cache-control", "no-cache",
		":authority", "www.example.com",
	}, 164)
	ReleaseHPACK(hpack)
}

func TestHPACKWriteResponseWithoutHuffman(t *testing.T) { // without huffman
	r := []byte{
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
	hpack := AcquireHPACK()
	hpack.DisableCompression = true
	hpack.SetMaxTableSize(256)

	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	r = []byte{0x48, 0x03, 0x33, 0x30, 0x37, 0xc1, 0xc0, 0xbf}
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

	r = []byte{
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

	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func TestHPACKWriteResponseWithHuffman(t *testing.T) { // WithHuffman
	r := []byte{
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

	hpack := AcquireHPACK()
	hpack.SetMaxTableSize(256)
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "302",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
		":status", "302",
	}, 222)

	r = []byte{0x48, 0x83, 0x64, 0x0e, 0xff, 0xc1, 0xc0, 0xbf}
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "307",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"location", "https://www.example.com",
	}, []string{
		":status", "307",
		"location", "https://www.example.com",
		"date", "Mon, 21 Oct 2013 20:13:21 GMT",
		"cache-control", "private",
	}, 222)

	r = []byte{
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
	writeHPACKAndCheck(t, hpack, r, []string{
		":status", "200",
		"cache-control", "private",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
		"location", "https://www.example.com",
		"content-encoding", "gzip",
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
	}, []string{
		"set-cookie", "foo=ASDJKHQKBZXOQWEOPIUAXQWEOIU; max-age=3600; version=1",
		"content-encoding", "gzip",
		"date", "Mon, 21 Oct 2013 20:13:22 GMT",
	}, 215)

	ReleaseHPACK(hpack)
}

func hexComparision(b, r []byte) (s string) {
	s += "\n"
	for i := range b {
		s += fmt.Sprintf("%x", b[i]) + " "
	}
	s += "\n"
	for i := range r {
		s += fmt.Sprintf("%x", r[i]) + " "
	}
	return
}
