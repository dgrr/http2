package http2

import (
	"bufio"
	"bytes"
	"io"
	"testing"

	"github.com/domsolutions/http2/http2utils"
)

const (
	testStr = "make fasthttp great again"
)

func TestFrameWrite(t *testing.T) {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	data := AcquireFrame(FrameData).(*Data)

	fr.SetBody(data)

	n, err := io.WriteString(data, testStr)
	if err != nil {
		t.Fatal(err)
	}
	if nn := len(testStr); n != nn {
		t.Fatalf("unexpected size %d<>%d", n, nn)
	}

	var bf = bytes.NewBuffer(nil)
	var bw = bufio.NewWriter(bf)
	fr.WriteTo(bw)
	bw.Flush()

	b := bf.Bytes()
	if str := string(b[9:]); str != testStr {
		t.Fatalf("mismatch %s<>%s", str, testStr)
	}
}

func TestFrameRead(t *testing.T) {
	var h [9]byte
	bf := bytes.NewBuffer(nil)
	br := bufio.NewReader(bf)

	http2utils.Uint24ToBytes(h[:3], uint32(len(testStr)))

	n, err := bf.Write(h[:9])
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Fatalf("unexpected written bytes %d<>9", n)
	}

	n, err = io.WriteString(bf, testStr)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(testStr) {
		t.Fatalf("unexpected written bytes %d<>%d", n, len(testStr))
	}

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	nn, err := fr.ReadFrom(br)
	if err != nil {
		t.Fatal(err)
	}
	n = int(nn)
	if n != len(testStr)+9 {
		t.Fatalf("unexpected read bytes %d<>%d", n, len(testStr)+9)
	}

	if fr.Type() != FrameData {
		t.Fatalf("unexpected frame type: %s. Expected Data", fr.Type())
	}

	data := fr.Body().(*Data)

	if str := string(data.Data()); str != testStr {
		t.Fatalf("mismatch %s<>%s", str, testStr)
	}
}

// TODO: continue
