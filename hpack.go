package http2

import (
	"bufio"
	"bytes"
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
	fields []*HeaderField

	// dynamic represents the dynamic table
	dynamic  []*HeaderField
	tableLen uint64

	tableSize    int
	maxTableSize int
}

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPack{}
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
	for _, hf := range hpack.dynamic {
		ReleaseHeaderField(hf)
	}
	hpack.dynamic = hpack.dynamic[:0]
	hpack.fields = hpack.fields[:0]
	hpack.tableLen = 0
	hpack.tableSize = 0
	hpack.maxTableSize = 0
}

// SetMaxTableSize sets the maximum dynamic table size.
func (hpack *HPack) SetMaxTableSize(size int) {
	hpack.maxTableSize = size
}

func (hpack *HPack) add(hf *HeaderField) {
	// Create a copy
	hf2 := AcquireHeaderField()
	hf.CopyTo(hf2)
	// TODO: Check if already exists
	hpack.dynamic = append(hpack.dynamic, hf2)
	hpack.tableLen++
}

// Peek returns HeaderField value of the given name.
//
// value will be nil if name is not found.
func (hpack *HPack) Peek(name string) (value []byte) {
	for _, hf := range hpack.fields {
		if b2s(hf.name) == name {
			value = hf.value
			break
		}
	}
	return
}

// PeekBytes returns HeaderField value of the given name in bytes.
//
// value will be nil if name is not found.
func (hpack *HPack) PeekBytes(name []byte) (value []byte) {
	for _, hf := range hpack.fields {
		if bytes.Equal(hf.name, name) {
			value = hf.value
			break
		}
	}
	return
}

// PeekField returns HeaderField structure of the given name.
//
// hf will be nil in case name is not found.
func (hpack *HPack) PeekField(name string) (hf *HeaderField) {
	// TODO: hf must be a copy or pointer?
	for _, hf2 := range hpack.fields {
		if b2s(hf2.name) == name {
			hf = hf2
			break
		}
	}
	return
}

// PeekFieldBytes returns HeaderField structure of the given name in bytes.
//
// hf will be nil in case name is not found.
func (hpack *HPack) PeekFieldBytes(name []byte) (hf *HeaderField) {
	// TODO: hf must be a copy or pointer?
	for _, hf2 := range hpack.fields {
		if bytes.Equal(hf2.name, name) {
			hf = hf2
			break
		}
	}
	return
}

// peek returns HeaderField from static or dynamic table.
//
// n must be the index in the table.
func (hpack *HPack) peek(n uint64) (hf *HeaderField) {
	// TODO: Change peek function name
	if n > maxIndex { // search in dynamic table
		nn := n - maxIndex - 1
		if nn < hpack.tableLen {
			hf = hpack.dynamic[nn]
		}
	} else {
		hf = staticTable[n-1] // must sub 1 because of indexing
	}
	return
}

// find gets the index of existent name in static or dynamic tables.
func (hpack *HPack) find(name []byte) (n uint64) {
	for i := 0; i < len(staticTable); i++ {
		if bytes.Equal(staticTable[i].name, name) {
			n = uint64(i + 1) // must add 1 because of indexing
			break
		}
	}
	if n == 0 {
		for i, v := range hpack.dynamic {
			if bytes.Equal(v.name, name) {
				n = uint64(i + maxIndex)
				break
			}
		}
	}
	return
}

// Read reads header fields from br and stores in hpack.
//
// This function must receive the payload of Header frame.
func (hpack *HPack) Read(b []byte) ([]byte, error) {
	// TODO: Change Read to Write?
	var (
		n          uint64
		c          byte
		err        error
		mustDecode bool
		hf         *HeaderField
	)
	for len(b) > 0 {
		c = b[0]
		switch {
		// Indexed Header Field.
		// The value must be indexed in the static or the dynamic table.
		// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
		case c&128 == 128:
			b, n, err = readInt(7, b)
			if err == nil {
				if hf2 := hpack.peek(n); hf2 != nil {
					hf = AcquireHeaderField()
					hf2.CopyTo(hf)
				}
			}

		// Literal Header Field with Incremental Indexing.
		// Name can be indexed or not. So if the first byte is equal to 64
		// the name value must be appended to the dynamic table.
		// https://tools.ietf.org/html/rfc7541#section-6.1
		case c&64 == 64:
			hf = AcquireHeaderField()
			// Reading name
			if c != 64 { // Read name as index
				b, n, err = readInt(6, b)
				if err == nil {
					if hf2 := hpack.peek(n); hf2 != nil {
						hf.SetNameBytes(hf2.name)
					} else {
						// TODO: error
					}
				}
			} else { // Read name literal string
				// Huffman encoded or not
				mustDecode = (b[0]&128 == 128)
				b, n, err = readInt(7, b)
				if err == nil {
					if !mustDecode {
						hf.SetNameBytes(b[:n])
					} else {
						bb := bytePool.Get().([]byte)
						bb = HuffmanDecode(bb[:0], b[:n])
						hf.SetNameBytes(bb)
						bytePool.Put(bb)
					}
					b = b[n:]
				}
			}
			// Reading value
			if err == nil {
				mustDecode = (b[0]&128 == 128)
				b, n, err = readInt(7, b)
				if err == nil {
					if !mustDecode {
						hf.SetValueBytes(b[:n])
					} else {
						bb := bytePool.Get().([]byte)
						bb = HuffmanDecode(bb[:0], b[:n])
						hf.SetValueBytes(bb)
						bytePool.Put(bb)
					}
					b = b[n:]
				}
			}
			if hf != nil {
				// add to the table as RFC specifies.
				hpack.add(hf)
			}

		// Literal Header Field Never Indexed.
		// This field must not be indexed and must be marked as sensible.
		// https://tools.ietf.org/html/rfc7541#section-6.2.3
		case c&240 == 16:
			hf = AcquireHeaderField()
			hf.sensible = true
			fallthrough
		// Header Field without Indexing.
		// This header field must not be appended to the dynamic table.
		// https://tools.ietf.org/html/rfc7541#section-6.2.2
		case c&240 == 0:
			if hf == nil {
				hf = AcquireHeaderField()
			}
			// Reading name
			if c != 0 { // Reading name as index
				b, n, err = readInt(4, b)
				if err == nil {
					if hf2 := hpack.peek(n); hf2 != nil {
						hf.SetNameBytes(hf2.name)
					} else {
						// TODO: error
					}
				}
			} else { // Reading name as string literal
				mustDecode = (b[0]&128 == 128)
				b, n, err = readInt(7, b)
				if err == nil {
					if !mustDecode {
						hf.SetNameBytes(b[:n])
					} else {
						bb := bytePool.Get().([]byte)
						bb = HuffmanDecode(bb[:0], b[:n])
						hf.SetNameBytes(bb)
						bytePool.Put(bb)
					}
					b = b[n:]
				}
			}
			// Reading value
			if err == nil {
				mustDecode = (b[0]&128 == 128)
				b, n, err = readInt(7, b)
				if err == nil {
					if !mustDecode {
						hf.SetValueBytes(b[:n])
					} else {
						bb := bytePool.Get().([]byte)
						bb = HuffmanDecode(bb[:0], b[:n])
						hf.SetNameBytes(bb)
						bytePool.Put(bb)
					}
					b = b[n:]
				}
			}

		// Dynamic Table Size Update
		// Changes the size of the dynamic table.
		// https://tools.ietf.org/html/rfc7541#section-6.3
		case c&32 == 32:
			// TODO: xd
		}

		if err != nil {
			if hf != nil {
				ReleaseHeaderField(hf)
			}
			break
		}

		if hf != nil {
			hpack.fields = append(hpack.fields, hf)
			hf = nil
		}
	}
	return b, err
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

func readIntFrom(n int, br *bufio.Reader) (nn uint64, err error) {
	var b byte
	b, err = br.ReadByte()
	if err == nil {
		nu := uint64(1<<uint64(n) - 1)
		nn = uint64(b)
		nn &= nu
		if nn < nu {
			return
		}
		nn = 0
		i := 1
		m := uint64(0)
		for {
			b, err = br.ReadByte()
			if err != nil {
				break
			}
			nn |= (uint64(b&127) << m)
			m += 7
			if m > 63 {
				err = ErrBitOverflow
				break
			}
			i++
			if b&128 != 128 {
				break
			}
		}
		nn += nu
	}
	return
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

func (hpack *HPack) WriteTo(bw io.Writer) (int64, error) {
	// TODO: Replace writeHeaderField with this function.
	// and write all hpack header fields to bw.
	return 0, nil
}

var staticTable = []*HeaderField{ // entry + 1
	&HeaderField{name: []byte(":authority")}, // 1
	&HeaderField{name: []byte(":method"), value: []byte("GET")},
	&HeaderField{name: []byte(":method"), value: []byte("POST")},
	&HeaderField{name: []byte(":path"), value: []byte("/")},
	&HeaderField{name: []byte(":path"), value: []byte("/index.html")},
	&HeaderField{name: []byte(":scheme"), value: []byte("http")},
	&HeaderField{name: []byte(":scheme"), value: []byte("https")},
	&HeaderField{name: []byte(":status"), value: []byte("200")},
	&HeaderField{name: []byte(":status"), value: []byte("204")},
	&HeaderField{name: []byte(":status"), value: []byte("206")},
	&HeaderField{name: []byte(":status"), value: []byte("304")},
	&HeaderField{name: []byte(":status"), value: []byte("400")},
	&HeaderField{name: []byte(":status"), value: []byte("404")},
	&HeaderField{name: []byte(":status"), value: []byte("500")},
	&HeaderField{name: []byte("accept-charset")},
	&HeaderField{name: []byte("accept-encoding"), value: []byte("gzip, deflate")},
	&HeaderField{name: []byte("accept-language")},
	&HeaderField{name: []byte("accept-ranges")},
	&HeaderField{name: []byte("accept")},
	&HeaderField{name: []byte("access-control-allow-origin")},
	&HeaderField{name: []byte("age")},
	&HeaderField{name: []byte("allow")},
	&HeaderField{name: []byte("authorization")},
	&HeaderField{name: []byte("cache-control")},
	&HeaderField{name: []byte("content-disposition")},
	&HeaderField{name: []byte("content-encoding")},
	&HeaderField{name: []byte("content-language")},
	&HeaderField{name: []byte("content-length")},
	&HeaderField{name: []byte("content-location")},
	&HeaderField{name: []byte("content-range")},
	&HeaderField{name: []byte("content-type")},
	&HeaderField{name: []byte("cookie")},
	&HeaderField{name: []byte("date")},
	&HeaderField{name: []byte("etag")},
	&HeaderField{name: []byte("expect")},
	&HeaderField{name: []byte("expires")},
	&HeaderField{name: []byte("from")},
	&HeaderField{name: []byte("host")},
	&HeaderField{name: []byte("if-match")},
	&HeaderField{name: []byte("if-modified-since")},
	&HeaderField{name: []byte("if-none-match")},
	&HeaderField{name: []byte("if-range")},
	&HeaderField{name: []byte("if-unmodified-since")},
	&HeaderField{name: []byte("last-modified")},
	&HeaderField{name: []byte("link")},
	&HeaderField{name: []byte("location")},
	&HeaderField{name: []byte("max-forwards")},
	&HeaderField{name: []byte("proxy-authenticate")},
	&HeaderField{name: []byte("proxy-authorization")},
	&HeaderField{name: []byte("range")},
	&HeaderField{name: []byte("referer")},
	&HeaderField{name: []byte("refresh")},
	&HeaderField{name: []byte("retry-after")},
	&HeaderField{name: []byte("server")},
	&HeaderField{name: []byte("set-cookie")},
	&HeaderField{name: []byte("strict-transport-security")},
	&HeaderField{name: []byte("transfer-encoding")},
	&HeaderField{name: []byte("user-agent")},
	&HeaderField{name: []byte("vary")},
	&HeaderField{name: []byte("via")},
	&HeaderField{name: []byte("www-authenticate")}, // 61
}

// maxIndex defines the maximum index number of the static table.
const maxIndex = 61
