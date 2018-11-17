package http2

import (
	"errors"
	"io"
	"sync"
)

// HeaderField represents a field in HPACK tables.
//
// Use AcquireHeaderField to acquire HeaderField.
type HeaderField struct {
	name, value []byte
	sensible    bool
}

var headerPool = sync.Pool{
	New: func() interface{} {
		return &HeaderField{}
	},
}

// AcquireHeaderField gets HeaderField from the pool.
func AcquireHeaderField() *HeaderField {
	return headerPool.Get().(*HeaderField)
}

// ReleaseHeaderField puts HeaderField to the pool.
func ReleaseHeaderField(hf *HeaderField) {
	hf.Reset()
	headerPool.Put(hf)
}

// Reset resets header field values.
func (hf *HeaderField) Reset() {
	hf.name = hf.name[:0]
	hf.value = hf.value[:0]
	hf.sensible = false
}

// CopyTo copies hf to hf2
func (hf *HeaderField) CopyTo(hf2 *HeaderField) {
	hf2.name = append(hf2.name[:0], hf.name...)
	hf2.value = append(hf2.value[:0], hf.value...)
	hf2.sensible = hf.sensible
}

// Name returns the name of the field
func (hf *HeaderField) Name() string {
	return string(hf.name)
}

// Value returns the value of the field
func (hf *HeaderField) Value() string {
	return string(hf.value)
}

// NameBytes returns the name bytes of the field.
func (hf *HeaderField) NameBytes() []byte {
	return hf.name
}

// ValueBytes returns the value bytes of the field.
func (hf *HeaderField) ValueBytes() []byte {
	return hf.value
}

// SetName sets name b to the field.
func (hf *HeaderField) SetName(b string) {
	hf.name = append(hf.name[:0], b...)
}

// SetValue sets value b to the field.
func (hf *HeaderField) SetValue(b string) {
	hf.value = append(hf.value[:0], b...)
}

// SetNameBytes sets name bytes b to the field.
func (hf *HeaderField) SetNameBytes(b []byte) {
	hf.name = append(hf.name[:0], b...)
}

// SetValueBytes sets value bytes b to the field.
func (hf *HeaderField) SetValueBytes(b []byte) {
	hf.value = append(hf.value[:0], b...)
}

// IsPseudo returns true if field is pseudo header
func (hf *HeaderField) IsPseudo() bool {
	return len(hf.name) > 0 && hf.name[0] == ':'
}

// IsSensible returns if header field have been marked as sensible.
func (hf *HeaderField) IsSensible() bool {
	return hf.sensible
}

// https://tools.ietf.org/html/rfc7541#section-6
type binaryFormat uint8

const (
	// https://tools.ietf.org/html/rfc7541#section-6.1
	indexed binaryFormat = iota
	// https://tools.ietf.org/html/rfc7541#section-6.2
	literalIndexed
	// https://tools.ietf.org/html/rfc7541#section-6.3
	literalNoIndexed
	// https://tools.ietf.org/html/rfc7541#section-6.4
	literalNeverIndexed
)

// HPack represents header compression methods to
// encode and decode header fields in HTTP/2.
//
// HPack is the same as HTTP/1.1 header.
//
// Use AcquireHPack to acquire new HPack structure
// TODO: HPack to Headers?
type HPack struct {
	noCopy noCopy

	// fields are the header fields
	fields []HeaderField

	// dynamic represents the dynamic table
	dynamic      map[uint64]*HeaderField
	tableSize    int
	maxTableSize int
}

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPack{
			dynamic: make(map[uint64]*HeaderField),
		}
	},
}

// AcquireHPack gets HPack from pool
func AcquireHPack() *HPack {
	return hpackPool.Get().(*HPack)
}

// ReleaseHPack puts HPack to the pool
func ReleaseHPack(hpack *HPack) {
	hpack.Reset()
	hpackPool.Put(hpack)
}

// Reset deletes and realeases all dynamic header fields
func (hpack *HPack) Reset() {
	for k, hf := range hpack.dynamic {
		ReleaseHeaderField(hf)
		delete(hpack.dynamic, k)
	}
	hpack.tableSize = 0
	hpack.maxTableSize = 0
}

// SetMaxTableSize sets the maximum dynamic table size.
func (hpack *HPack) SetMaxTableSize(size int) {
	hpack.maxTableSize = size
}

// Parse parses header from b.
// Returned values are the new header, header field and/or error.
//
// It's safe to do ReleaseHeaderField after Parse call.
// If len(b) == 0 returns io.EOF.
func (hpack *HPack) Parse(b []byte) ([]byte, *HeaderField, error) {
	if len(b) == 0 {
		return b, nil, io.EOF
	}

	var i uint64
	var err error
	var hf *HeaderField

	c := b[0]
	switch {
	// An indexed header field representation identifies an
	// field in either the static table or the fields table
	// This index values are limited to 7 bits (2 ^ 7 = 128)
	// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
	case c&128 == 128: // 10000000 | 7 bits
		var field *HeaderField
		b, i, err = readInt(7, b)
		if i < uint64(len(staticTable)) {
			field = &staticTable[i-1]
		} else {
			field = hpack.dynamic[i]
		}
		if field == nil {
			return b, hf, errHeaderFieldNotFound
		}
		hf = AcquireHeaderField()
		field.CopyTo(hf)

	// A literal header field with incremental indexing representation
	// starts with the '01' 2-bit pattern.
	// https://httpwg.org/specs/rfc7541.html#literal.header.with.incremental.indexing
	case c&192 == 64: // 11000000 | 6 bits
		b, i, err = readInt(6, b)
		if err == nil {
			hf = AcquireHeaderField()
			b, err = hpack.readHeaderField(i, b, hf)
			// A literal header field with incremental indexing representation
			// results in appending a header field to the decoded header
			// list and inserting it as a new field into the fields table.
			if err == nil {
				// TODO: Check and increment table size
				field := AcquireHeaderField()
				hf.CopyTo(field)
				hpack.dynamic[i] = field
			}
		}
	// A literal header field without indexing representation
	// results in appending a header field to the decoded header
	// list without altering the fields table.
	// https://httpwg.org/specs/rfc7541.html#literal.header.without.indexing
	case c&240 == 0: // 11110000 | 4 bits
		b, i, err = readInt(4, b)
		if err == nil {
			hf = AcquireHeaderField()
			b, err = hpack.readHeaderField(i, b, hf)
		}

	// A literal header field never-indexed representation
	// results in appending a header field to the decoded header
	// list without altering the fields table.
	// Intermediaries MUST use the same representation for encoding this header field.
	// https://httpwg.org/specs/rfc7541.html#literal.header.never.indexed
	case c&240 == 16: // 11110000 | 4 bits
		b, i, err = readInt(4, b)
		if err == nil {
			hf = AcquireHeaderField()
			b, err = hpack.readHeaderField(i, b, hf)
			if hf != nil {
				hf.sensible = true
			}
		}
	case c&224 == 32:
		// TODO: Update
	default:
		err = errors.New("not found")
	}
	if err != nil && hf != nil {
		ReleaseHeaderField(hf)
		hf = nil
	}
	return b, hf, err
}

// readInt reads int type from header field.
// https://tools.ietf.org/html/rfc7541#section-5.1
func readInt(n int, b []byte) ([]byte, uint64, error) {
	nu := uint64(1<<uint64(n) - 1)
	nn := uint64(b[0])
	nn &= nu
	if nn < nu {
		return b[1:], nn, nil
	}

	nn = 0
	i := 1
	m := uint64(0)
	for i < len(b) {
		c := b[i]
		nn |= (uint64(c&127) << m)
		m += 7
		if m > 63 {
			return b[i:], 0, ErrBitOverflow
		}
		i++
		if c&128 != 128 {
			break
		}
	}
	return b[i:], nn + nu, nil
}

// writeInt writes int type to header field.
// https://tools.ietf.org/html/rfc7541#section-5.1
func writeInt(dst []byte, n uint8, nn uint64) []byte {
	nu := uint64(1<<n - 1)
	if nn < nu {
		dst[0] = byte(nn)
	} else {
		// TODO: Grow slice efficiently
		dst = dst[:cap(dst)]
		if i := 8 - len(dst); i > 0 {
			dst = append(dst, make([]byte, i)...)
		}
		nn -= nu
		dst[0] = byte(nu)
		nu = 1 << (n + 1)
		i := 0
		for nn > 0 {
			i++
			dst[i] = byte(nn | 128)
			nn >>= 7
		}
		dst[i] &= 127
	}
	return dst
}

// readString reads string from a header field.
// https://tools.ietf.org/html/rfc7541#section-5.2
func readString(dst, b []byte) ([]byte, []byte, error) {
	if b[0] > 126 {
		return dst, b, errors.New("error") // TODO: Define error
	}

	var n uint64
	var err error
	mustDecode := (b[0]&128 == 128) // huffman encoded
	b, n, err = readInt(7, b)
	if err != nil {
		return dst, b, err
	}
	if mustDecode {
		dst = HuffmanDecode(dst, b[:n])
	} else {
		dst = append(dst[:0], b[:n]...)
	}
	b = b[n:]
	return dst, b, err
}

// writeString writes string to a header field.
// https://tools.ietf.org/html/rfc7541#section-5.2
func writeString(dst, src []byte) []byte {
	// TODO: Reduce allocations
	edst := HuffmanEncode(nil, src)
	n := uint64(len(edst))
	nn := len(dst)
	dst = writeInt(dst, 7, n)
	dst = append(dst, edst...)
	dst[nn] |= 128
	edst = nil
	return dst
}

var errHeaderFieldNotFound = errors.New("Indexed field not found")

// TODO: Make public radHeaderField and writeHeaderField preparing it to be used correctly xd.

func (hpack *HPack) readHeaderField(i uint64, b []byte, hf *HeaderField) ([]byte, error) {
	// TODO:
	var err error
	if i == 0 {
		b, hf.name, err = readString(b, hf.name)
		if err == nil {
			b, hf.value, err = readString(b, hf.value)
		}
	} else {
		var field *HeaderField
		if i < uint64(len(staticTable)) {
			field = &staticTable[i-1]
		} else {
			field = hpack.dynamic[i]
		}
		if field == nil {
			return b, errHeaderFieldNotFound
		}

		hf.SetNameBytes(field.name)
		b, hf.value, err = readString(b, hf.value)
	}
	return b, err
}

func (hpack *HPack) WriteTo(bw io.Writer) (int64, error) {
	// TODO: Replace writeHeaderField with this function.
	// and write all hpack header fields to bw.
	return 0, nil
}

// TODO: Add sensible header fields
func (hpack *HPack) writeHeaderField(dst []byte, hf *HeaderField, n uint8, index uint64) []byte {
	if index > 0 {
		dst = writeInt(dst, n, index)
		if n < 7 && len(hf.value) > 0 {
			dst = writeString(dst, hf.value)
		}
	} else {
		dst = writeString(dst, hf.name)
		dst = writeString(dst, hf.value)
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
