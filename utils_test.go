package http2

import (
	"testing"
)

func TestCutPadding(t *testing.T) {
	str := []byte{13}
	str = append(str, "8971293nfasv7asnrnqw9bma 237urkf8KifgiMKFG98UIM8fgnb kifgnrA7JKLK"...)
	nlen := uint32(len(str) - 13)

	fr := Frame{
		length:  uint32(len(str)),
		payload: str,
		flags:   FlagPadded,
	}

	p := cutPadding(&fr)
	if len(p) != int(nlen)-1 {
		t.Fatalf("unexpected len: %d<>%d", len(p), nlen)
	}
}

func BenchmarkCutPadding(b *testing.B) {
	str := []byte{17}
	str = append(str, "8971293nfasv7asnrnqw9bma 237urkf8KifgiMKFG98UIM8fgnb kifgnrA7JKLK"...)
	fr := Frame{
		length:  uint32(len(str) - 17),
		payload: str,
		flags:   FlagPadded,
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := cutPadding(&fr)
		if len(p) == 0 {
			b.Fatal("wrong cutting")
		}
	}
}
