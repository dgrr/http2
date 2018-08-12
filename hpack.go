package fasthttp2

import (
	"errors"
	"io"
	"strconv"
	"sync"
)

// HPack represents header compression methods to
// encode and decode header fields in HTTP/2
//
// Use AcquireHPack to acquire new HPack structure
type HPack struct {
	static  []argsKV
	dynamic map[uint64]argsKV
}

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPack{
			static:  staticTable,
			dynamic: make(map[uint64]argsKV),
		}
	},
}

func AcquireHPack() *HPack {
	hpack := hpackPool.Get().(*HPack)
	return hpack
}

func ReleaseHPack(hpack *HPack) {
	hpack.Reset()
	hpackPool.Put(hpack)
}

func (hpack *HPack) Reset() {
	for k, _ := range hpack.dynamic {
		delete(hpack.dynamic, k)
	}
}

func (hpack *HPack) next(b, k, v []byte) ([]byte, []byte, []byte, error) {
	if len(b) == 0 {
		return b, k, v, io.EOF
	}
	var i uint64
	var err error
	c := b[0]
	switch {
	// An indexed header field representation identifies an
	// entry in either the static table or the dynamic table
	// This index values are limited to 7 bits (2 ^ 7 = 128)
	// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
	case c&128 == 128: // 10000000 | 7 bits
		var entry argsKV
		b, i, err = readInt(7, b)
		if i < uint64(len(hpack.static)) {
			entry = hpack.static[i-1]
		} else {
			var ok bool
			entry, ok = hpack.dynamic[i]
			if !ok {
				return b, k, v, errFieldNotFound
			}
		}
		k = append(k, entry.key...)
		v = append(v, entry.value...)
	// A literal header field with incremental indexing representation
	// starts with the '01' 2-bit pattern.
	// https://httpwg.org/specs/rfc7541.html#literal.header.with.incremental.indexing
	case c&192 == 64: // 11000000 | 6 bits
		b, i, err = readInt(6, b)
		if err == nil {
			b, k, v, err = hpack.readField(i, b, k, v)
			// A literal header field with incremental indexing representation
			// results in appending a header field to the decoded header
			// list and inserting it as a new entry into the dynamic table.
			hpack.dynamic[i] = argsKV{
				key:   k,
				value: v,
			}
		}
	// https://httpwg.org/specs/rfc7541.html#literal.header.without.indexing
	case c&240 == 0: // 11110000 | 4 bits
		b, i, err = readInt(4, b)
		if err == nil {
			b, k, v, err = hpack.readField(i, b, k, v)
		}
		// A literal header field without indexing representation
		// results in appending a header field to the decoded header
		// list without altering the dynamic table.

	// https://httpwg.org/specs/rfc7541.html#literal.header.never.indexed
	case c&240 == 16: // 11110000 | 4 bits
		b, i, err = readInt(4, b)
		if err == nil {
			b, k, v, err = hpack.readField(i, b, k, v)
		}
		// A literal header field never-indexed representation
		// results in appending a header field to the decoded header
		// list without altering the dynamic table.
		// Intermediaries MUST use the same representation for encoding this header field.
		// TODO: Implement sensitive header fields

	case c&224 == 32:
		// TODO: Update
	default:
		err = errors.New("not found")
	}
	return b, k, v, err
}

func (hpack *HPack) appendStatus(dst []byte, status int) []byte {
	n := 0
	switch status {
	case StatusOK:
		n = 8
	case StatusNoContent:
		n = 9
	case StatusPartialContent:
		n = 10
	case StatusNotModified:
		n = 11
	case StatusBadRequest:
		n = 12
	case StatusNotFound:
		n = 13
	case StatusInternalServerError:
		n = 14
	default:
		return hpack.writeField(dst, nil, s2b(strconv.Itoa(status)), 4, 8)
	}
	return hpack.writeField(dst, nil, nil, 7, uint64(n))
}

func appendServer(dst, server []byte) []byte {
	dst = writeInt(dst, 4, 54)
	dst = writeString(dst, server)
	return dst
}

func readInt(n int, b []byte) ([]byte, uint64, error) {
	nn := 1
	num := uint64(b[0])
	num &= (1 << uint64(n)) - 1
	if num < (1<<uint64(n))-1 {
		return b[1:], num, nil
	}
	var m uint64
	for nn < len(b) {
		c := b[nn]
		nn++
		num += uint64(c&127) << m
		if c&128 != 128 {
			break
		}
		m += 7
		if m >= 63 {
			return b[nn:], 0, errBitOverflow
		}
	}
	return b[nn:], num, nil
}

func writeInt(dst []byte, n uint, i uint64) []byte {
	b := uint64(1<<n) - 1
	if i < b {
		dst = append(dst, byte(i))
	} else {
		dst = append(dst, byte(b))
		i -= b
		for i >= 128 {
			dst = append(dst, byte(0x80|(i&0x7f)))
			i >>= 7
		}
		dst = append(dst, byte(i))
	}
	return dst
}

func readString(b, s []byte) ([]byte, []byte, error) {
	var length uint64
	var err error
	mustDecode := b[0]&128 == 128 // huffman encoded
	b, length, err = readInt(7, b)
	if err != nil {
		return b, s, err
	}
	if mustDecode {
		s = huffmanDecode(s, b[:length])
	} else {
		s = append(s[:0], b[:length]...)
	}
	b = b[length:]
	return b, s, err
}

func writeString(dst, src []byte) []byte {
	// TODO
	edst := huffmanEncode(nil, src)
	n := uint64(len(edst))
	nn := len(dst)
	dst = writeInt(dst, 7, n)
	dst = append(dst, edst...)
	dst[nn] |= 128
	edst = nil
	return dst
}

var errFieldNotFound = errors.New("Indexed field not found")

func (hpack *HPack) readField(i uint64, b, k, v []byte) ([]byte, []byte, []byte, error) {
	var err error
	if i == 0 {
		b, k, err = readString(b, k)
		if err == nil {
			b, v, err = readString(b, v)
		}
	} else {
		var entry argsKV
		if i < uint64(len(hpack.static)) {
			entry = hpack.static[i-1]
		} else {
			var ok bool
			entry, ok = hpack.dynamic[i]
			if !ok {
				return b, k, v, errFieldNotFound
			}
		}
		k = append(k[:0], entry.key...)
		b, v, err = readString(b, v)
	}
	return b, k, v, err
}

// TODO: Add sensible header fields
func (hpack *HPack) writeField(dst, k, v []byte, n uint, index uint64) []byte {
	if index > 0 {
		dst = writeInt(dst, n, index)
		if n < 7 && len(v) > 0 {
			dst = writeString(dst, v)
		}
	} else {
		// TODO: Search in dynamic table
		dst = writeString(dst, k)
		dst = writeString(dst, v)
	}
	return dst
}
