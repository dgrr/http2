package http2

import (
	"bufio"
	"bytes"
	"testing"
)

// The parsers below all take bytes straight off the wire from an unauthenticated
// peer, which makes them the parts worth fuzzing. Each target asserts the same
// contract: whatever the input, the parser either returns an error or a usable
// result, and never panics or runs away.

func FuzzHuffmanDecode(f *testing.F) {
	f.Add(encodedBytes)
	f.Add(littleEncodedBytes)
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, src []byte) {
		dst, err := HuffmanDecode(nil, src)
		if err != nil {
			return
		}

		// A successful decode has to round trip: re-encoding what came out
		// must decode back to the same bytes.
		again, err := HuffmanDecode(nil, HuffmanEncode(nil, dst))
		if err != nil {
			t.Fatalf("re-decoding a successful decode failed: %v", err)
		}

		if !bytes.Equal(dst, again) {
			t.Fatalf("round trip changed the value: %q != %q", dst, again)
		}
	})
}

func FuzzHPACKNext(f *testing.F) {
	// A handful of well-formed blocks: indexed field, literal with incremental
	// indexing, and a literal with a huffman-coded value.
	f.Add([]byte{0x82})
	f.Add([]byte{0x40, 0x01, 'a', 0x01, 'b'})
	f.Add([]byte{0x00, 0x01, 'a', 0x81, 0x3f})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, block []byte) {
		hp := &HPACK{}
		hp.Reset()

		hf := &HeaderField{}

		b := block

		// Bound the loop by the input: every successful field must consume at
		// least one byte, so a parser that returns nil error without advancing
		// is a bug worth catching rather than an infinite loop in the fuzzer.
		for i := 0; len(b) > 0; i++ {
			if i > len(block) {
				t.Fatalf("decoded %d fields from %d bytes without consuming input", i, len(block))
			}

			before := len(b)

			var err error

			b, err = hp.Next(hf, b)
			if err != nil {
				return
			}

			if len(b) >= before {
				t.Fatalf("field %d consumed no input (%d -> %d bytes)", i, before, len(b))
			}
		}
	})
}

func FuzzFrameHeaderRead(f *testing.F) {
	f.Add([]byte{0, 0, 0, byte(FrameSettings), 0, 0, 0, 0, 0})
	f.Add([]byte{0, 0, 8, byte(FramePing), 0, 0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{0, 0, 4, byte(FrameWindowUpdate), 0, 0, 0, 0, 1, 0, 0, 0, 1})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, raw []byte) {
		br := bufio.NewReader(bytes.NewReader(raw))

		fr, err := ReadFrameFrom(br)
		if err != nil {
			return
		}

		defer ReleaseFrameHeader(fr)

		// A frame that parsed must report a length no larger than the input it
		// came from, otherwise it is claiming payload it never read.
		if fr.Len() > len(raw) {
			t.Fatalf("frame reports %d payload bytes from a %d byte input", fr.Len(), len(raw))
		}

		if fr.Body() == nil {
			t.Fatal("frame parsed without a body")
		}
	})
}
