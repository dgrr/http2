package fasthttp2

import (
	"errors"
	"io"
	"strconv"
	"sync"
)

// HeaderField represents a field in HPACK tables.
type HeaderField struct {
	name, value []byte
	sensible    bool
}

// Name returns the name of the field
func (hf *HeaderField) Name() string {
	return string(f.name)
}

// Value returns the value of the field
func (hf *HeaderField) Value() string {
	return string(f.value)
}

// NameBytes returns the name bytes of the field.
func (hf *HeaderField) NameBytes() []byte {
	return f.name
}

// ValueBytes returns the value bytes of the field.
func (hf *HeaderField) ValueBytes() []byte {
	return f.value
}

// SetName sets name b to the field.
func (hf *HeaderField) SetName(b string) {
	f.name = append(f.name[:0], b...)
}

// SetValue sets value b to the field.
func (hf *HeaderField) SetValue(b string) {
	f.value = append(f.value[:0], b...)
}

// SetNameBytes sets name bytes b to the field.
func (hf *HeaderField) SetNameBytes(b []byte) {
	f.name = append(f.name[:0], b...)
}

// SetValueBytes sets value bytes b to the field.
func (hf *HeaderField) SetValueBytes(b []byte) {
	f.value = append(f.value[:0], b...)
}

// IsPseudo returns true if field is pseudo header
func (hf *HeaderField) IsPseudo() bool {
	return len(f.name) > 0 && f.name[0] == ':'
}

// HPack represents header compression methods to
// encode and decode header fields in HTTP/2
//
// Use AcquireHPack to acquire new HPack structure
type HPack struct {
	// fields represents dynamic table fields
	fields map[uint64]*HeaderField
}

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPack{
			fields: make(map[uint64]*HeaderFields),
		}
	},
}

func AcquireHPack() *HPack {
	return hpackPool.Get().(*HPack)
}

func ReleaseHPack(hpack *HPack) {
	hpack.Reset()
	hpackPool.Put(hpack)
}

func (hpack *HPack) Reset() {
	for k, _ := range hpack.fields {
		delete(hpack.fields, k)
	}
}

func (field *HeaderField) next(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return b, k, v, io.EOF
	}
	var i uint64
	var err error
	c := b[0]
	switch {
	// An indexed header field representation identifies an
	// entry in either the static table or the fields table
	// This index values are limited to 7 bits (2 ^ 7 = 128)
	// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
	case c&128 == 128: // 10000000 | 7 bits
		var entry HeaderField
		b, i, err = readInt(7, b)
		if i < uint64(len(staticTable)) {
			entry = staticTable[i-1]
		} else {
			var ok bool
			entry, ok = hpack.fields[i]
			if !ok {
				return b, k, v, errHeaderFieldNotFound
			}
		}
		k = append(k, entry.name...)
		v = append(v, entry.value...)
	// A literal header field with incremental indexing representation
	// starts with the '01' 2-bit pattern.
	// https://httpwg.org/specs/rfc7541.html#literal.header.with.incremental.indexing
	case c&192 == 64: // 11000000 | 6 bits
		b, i, err = readInt(6, b)
		if err == nil {
			b, k, v, err = hpack.readHeaderField(i, b, k, v)
			// A literal header field with incremental indexing representation
			// results in appending a header field to the decoded header
			// list and inserting it as a new entry into the fields table.
			hpack.fields[i] = HeaderField{
				name:  k,
				value: v,
			}
		}
	// https://httpwg.org/specs/rfc7541.html#literal.header.without.indexing
	case c&240 == 0: // 11110000 | 4 bits
		b, i, err = readInt(4, b)
		if err == nil {
			b, k, v, err = hpack.readHeaderField(i, b, k, v)
		}
		// A literal header field without indexing representation
		// results in appending a header field to the decoded header
		// list without altering the fields table.

	// https://httpwg.org/specs/rfc7541.html#literal.header.never.indexed
	case c&240 == 16: // 11110000 | 4 bits
		b, i, err = readInt(4, b)
		if err == nil {
			b, k, v, err = hpack.readHeaderField(i, b, k, v)
		}
		// A literal header field never-indexed representation
		// results in appending a header field to the decoded header
		// list without altering the fields table.
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
		return hpack.writeHeaderField(dst, nil, s2b(strconv.Itoa(status)), 4, 8)
	}
	return hpack.writeHeaderField(dst, nil, nil, 7, uint64(n))
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

var errHeaderFieldNotFound = errors.New("Indexed field not found")

func (hpack *HPack) readHeaderField(i uint64, b, k, v []byte) ([]byte, []byte, []byte, error) {
	var err error
	if i == 0 {
		b, k, err = readString(b, k)
		if err == nil {
			b, v, err = readString(b, v)
		}
	} else {
		var entry HeaderField
		if i < uint64(len(hpack.static)) {
			entry = hpack.static[i-1]
		} else {
			var ok bool
			entry, ok = hpack.fields[i]
			if !ok {
				return b, k, v, errHeaderFieldNotFound
			}
		}
		k = append(k[:0], entry.name...)
		b, v, err = readString(b, v)
	}
	return b, k, v, err
}

// TODO: Add sensible header fields
func (hpack *HPack) writeHeaderField(dst, k, v []byte, n uint, index uint64) []byte {
	if index > 0 {
		dst = writeInt(dst, n, index)
		if n < 7 && len(v) > 0 {
			dst = writeString(dst, v)
		}
	} else {
		// TODO: Search in fields table
		dst = writeString(dst, k)
		dst = writeString(dst, v)
	}
	return dst
}

var staticTable = []HeaderField{
	HeaderField{name: []byte(":authority")},
	HeaderField{name: []byte(":method"), value: []byte("GET")},
	HeaderField{name: []byte(":method"), value: []byte("POST")},
	HeaderField{name: []byte(":path"), value: []byte("/")},
	HeaderField{name: []byte(":path"), value: []byte("/index.html")},
	HeaderField{name: []byte(":scheme"), value: []byte("http")},
	HeaderField{name: []byte(":scheme"), value: []byte("https")},
	HeaderField{name: []byte(":status"), value: []byte("200")},
	HeaderField{name: []byte(":status"), value: []byte("204")},
	HeaderField{name: []byte(":status"), value: []byte("206")},
	HeaderField{name: []byte(":status"), value: []byte("304")},
	HeaderField{name: []byte(":status"), value: []byte("400")},
	HeaderField{name: []byte(":status"), value: []byte("404")},
	HeaderField{name: []byte(":status"), value: []byte("500")},
	HeaderField{name: []byte("accept-charset")},
	HeaderField{name: []byte("accept-encoding"), value: []byte("gzip, deflate")},
	HeaderField{name: []byte("accept-language")},
	HeaderField{name: []byte("accept-ranges")},
	HeaderField{name: []byte("accept")},
	HeaderField{name: []byte("access-control-allow-origin")},
	HeaderField{name: []byte("age")},
	HeaderField{name: []byte("allow")},
	HeaderField{name: []byte("authorization")},
	HeaderField{name: []byte("cache-control")},
	HeaderField{name: []byte("content-disposition")},
	HeaderField{name: []byte("content-encoding")},
	HeaderField{name: []byte("content-language")},
	HeaderField{name: []byte("content-length")},
	HeaderField{name: []byte("content-location")},
	HeaderField{name: []byte("content-range")},
	HeaderField{name: []byte("content-type")},
	HeaderField{name: []byte("cookie")},
	HeaderField{name: []byte("date")},
	HeaderField{name: []byte("etag")},
	HeaderField{name: []byte("expect")},
	HeaderField{name: []byte("expires")},
	HeaderField{name: []byte("from")},
	HeaderField{name: []byte("host")},
	HeaderField{name: []byte("if-match")},
	HeaderField{name: []byte("if-modified-since")},
	HeaderField{name: []byte("if-none-match")},
	HeaderField{name: []byte("if-range")},
	HeaderField{name: []byte("if-unmodified-since")},
	HeaderField{name: []byte("last-modified")},
	HeaderField{name: []byte("link")},
	HeaderField{name: []byte("location")},
	HeaderField{name: []byte("max-forwards")},
	HeaderField{name: []byte("proxy-authenticate")},
	HeaderField{name: []byte("proxy-authorization")},
	HeaderField{name: []byte("range")},
	HeaderField{name: []byte("referer")},
	HeaderField{name: []byte("refresh")},
	HeaderField{name: []byte("retry-after")},
	HeaderField{name: []byte("server")},
	HeaderField{name: []byte("set-cookie")},
	HeaderField{name: []byte("strict-transport-security")},
	HeaderField{name: []byte("transfer-encoding")},
	HeaderField{name: []byte("user-agent")},
	HeaderField{name: []byte("vary")},
	HeaderField{name: []byte("via")},
	HeaderField{name: []byte("www-authenticate")},
}
