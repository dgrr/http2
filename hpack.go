package http2

import (
	"bytes"
	"fmt"
	"sync"
)

// HPACK represents header compression methods to
// encode and decode header fields in HTTP/2.
//
// HPACK is equivalent to a HTTP/1 header.
//
// Use AcquireHPACK to acquire new HPACK structure
// TODO: HPACK to Headers?
type HPACK struct {
	// DisableCompression disables compression for literal header fields.
	DisableCompression bool

	// DisableDynamicTable disables the usage of the dynamic table for
	// the HPACK structure. If this option is true the HPACK won't add any
	// field to the dynamic table unless it was sended by the peer.
	//
	// This field was implemented because in many ways the server could modify
	// the fields stablished by the client losing performance calculated by client.
	DisableDynamicTable bool

	// dynamic represents the dynamic table
	dynamic []*HeaderField

	tableSize    int
	maxTableSize int
}

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPACK{
			maxTableSize: int(defaultHeaderTableSize),
			dynamic:      make([]*HeaderField, 0, 16),
		}
	},
}

// AcquireHPACK gets HPACK from pool
func AcquireHPACK() *HPACK {
	hpack := hpackPool.Get().(*HPACK)
	hpack.Reset()
	return hpack
}

// ReleaseHPACK puts HPACK to the pool
func ReleaseHPACK(hpack *HPACK) {
	hpackPool.Put(hpack)
}

func (hpack *HPACK) releaseDynamic() {
	for _, hf := range hpack.dynamic {
		ReleaseHeaderField(hf)
	}
	hpack.dynamic = hpack.dynamic[:0]
}

// Reset deletes and releases all dynamic header fields
func (hpack *HPACK) Reset() {
	hpack.releaseDynamic()
	hpack.tableSize = 0
	hpack.maxTableSize = int(defaultHeaderTableSize)
	hpack.DisableCompression = false
}

// SetMaxTableSize sets the maximum dynamic table size.
func (hpack *HPACK) SetMaxTableSize(size int) {
	hpack.maxTableSize = size
}

// Dynamic size returns the size of the dynamic table.
// https://tools.ietf.org/html/rfc7541#section-4.1
func (hpack *HPACK) DynamicSize() (n int) {
	for _, hf := range hpack.dynamic {
		n += hf.Size()
	}
	return
}

// add adds header field to the dynamic table.
func (hpack *HPACK) addDynamic(hf *HeaderField) {
	// TODO: Optimize using reverse indexes.

	// append a copy
	hf2 := AcquireHeaderField()
	hf.CopyTo(hf2)

	hpack.dynamic = append(hpack.dynamic, hf2)

	// checking table size
	hpack.shrink()
}

// shrink shrinks the dynamic table if needed.
func (hpack *HPACK) shrink() {
	n := 0 // elements to remove
	hpack.tableSize = hpack.DynamicSize()
	for i := range hpack.dynamic {
		if hpack.tableSize < hpack.maxTableSize {
			break
		}
		hpack.tableSize -= hpack.dynamic[i].Size()
		n++
	}

	for i := 0; i < n; i++ {
		// release the header field
		ReleaseHeaderField(hpack.dynamic[i])
		// shrinking slice
	}
	if n > 0 {
		hpack.dynamic = append(hpack.dynamic[:0], hpack.dynamic[n:]...)
	}
}

// peek returns HeaderField from static or dynamic table.
//
// n must be the index in the table.
func (hpack *HPACK) peek(n uint64) *HeaderField {
	var (
		index int
		table []*HeaderField
	)

	if n < maxIndex {
		index, table = int(n-1), staticTable
	} else { // search in dynamic table
		nn := len(hpack.dynamic) - int(n-maxIndex) - 1
		// dynamic_len = 11
		// n = 64
		// nn = 11 - 64 - 62 - 1 = 8
		index, table = nn, hpack.dynamic
	}

	if index < 0 {
		return nil
	}

	return table[index]
}

// find gets the index of existent key in static or dynamic tables.
func (hpack *HPACK) search(hf *HeaderField) (n uint64, fullMatch bool) {
	// start searching in the dynamic table (probably it contains less fields than the static.
	for i, hf2 := range hpack.dynamic {
		if fullMatch = bytes.Equal(hf.key, hf2.key) &&
			bytes.Equal(hf.value, hf2.value); fullMatch {
			n = uint64(maxIndex + len(hpack.dynamic) - i - 1)
			break
		}
	}

	if n == 0 {
		for i, hf2 := range staticTable {
			if bytes.Equal(hf.key, hf2.key) {
				if fullMatch = bytes.Equal(hf.value, hf2.value); fullMatch {
					n = uint64(i + 1)
					break
				}
				if n == 0 {
					n = uint64(i + 1)
				}
			}
		}
	}

	return
}

const (
	indexByte   = 128 // 10000000
	literalByte = 64  // 01000000
	noIndexByte = 240 // 11110000
)

var bytePool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 128)
	},
}

var ErrFieldNotFound = NewError(
	FlowControlError,
	"field not found in neither table")

// Next reads and process the content of `b`. If buf contains a valid HTTP/2 header
// the content will be parsed into `hf`.
//
// This function returns the next byte slice that should be read.
// `b` must be a valid payload coming from a Header frame.
func (hpack *HPACK) Next(hf *HeaderField, b []byte) ([]byte, error) {
	var (
		n   uint64
		c   byte
		err error
	)

loop:
	c = b[0]
	switch {
	// Indexed Header Field.
	// The value must be indexed in the static or the dynamic table.
	// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
	case c&indexByte == indexByte: // 1000 0000
		b, n = readInt(7, b)
		hf2 := hpack.peek(n)
		if hf2 == nil {
			return b, ErrFieldNotFound
		}

		hf2.CopyTo(hf)

	// Literal Header Field with Incremental Indexing.
	// Key can be indexed or not. Then appended to the table
	// https://tools.ietf.org/html/rfc7541#section-6.1
	case c&literalByte == literalByte: // 0100 0000
		// Reading key
		if c != 64 { // Read key as index
			b, n = readInt(6, b)

			hf2 := hpack.peek(n)
			if hf2 == nil {
				return b, ErrFieldNotFound
			}

			hf.SetKeyBytes(hf2.KeyBytes())
		} else { // Read key literal string
			b = b[1:]
			dst := bytePool.Get().([]byte)

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetKeyBytes(dst)
			}

			bytePool.Put(dst)
		}

		// Reading value
		if err == nil {
			if b[0] == c {
				b = b[1:]
			}

			dst := bytePool.Get().([]byte)

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetValueBytes(dst)
				// add to the table as RFC specifies.
				hpack.addDynamic(hf)
			}

			bytePool.Put(dst)
		}

	// Literal Header Field Never Indexed.
	// The value of this field must not be encoded
	// https://tools.ietf.org/html/rfc7541#section-6.2.3
	case c&noIndexByte == 16: // 0001 0000
		hf.sensible = true
		fallthrough
	// Header Field without Indexing.
	// This header field must not be appended to the dynamic table.
	// https://tools.ietf.org/html/rfc7541#section-6.2.2
	case c&noIndexByte == 0: // 0000 0000
		// Reading key
		if c != 0 { // Reading key as index
			b, n = readInt(4, b)
			hf2 := hpack.peek(n)
			if hf2 == nil {
				return b, ErrFieldNotFound
			}

			hf.SetKeyBytes(hf2.key)
		} else { // Reading key as string literal
			b = b[1:]
			dst := bytePool.Get().([]byte)

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetKeyBytes(dst)
			}

			bytePool.Put(dst)
		}

		// Reading value
		if err == nil {
			if b[0] == c {
				b = b[1:]
			}

			dst := bytePool.Get().([]byte)

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetValueBytes(dst)
			}

			bytePool.Put(dst)
		}

	// Dynamic Table Size Update
	// Changes the size of the dynamic table.
	// https://tools.ietf.org/html/rfc7541#section-6.3
	case c&32 == 32: // 001- ----
		b, n = readInt(5, b)
		hpack.maxTableSize = int(n)
		hpack.shrink()
		goto loop
	}

	return b, err
}

// readInt reads int type from header field.
// https://tools.ietf.org/html/rfc7541#section-5.1
func readInt(n int, b []byte) ([]byte, uint64) {
	// 1<<7 - 1 = 0111 1111
	b0 := byte(1<<n - 1)
	// if b[0] = 0111 1111 then continue reading the int
	// if not, then we are finished
	// if b0 is 0011 1111, then b0&b[0] != b0 = false
	if b0&b[0] != b0 {
		return b[1:], uint64(b[0] & b0)
	}

	nn := uint64(0)
	i := 1
	for i < len(b) {
		nn |= uint64(b[i]&127) << ((i - 1) * 7)
		if b[i]&128 != 128 {
			break
		}
		i++
	}

	return b[i+1:], nn + uint64(b0)
}

// appendInt appends int type to header field excluding the last byte
// which will be OR'ed.
// https://tools.ietf.org/html/rfc7541#section-5.1
func appendInt(dst []byte, bits uint8, index uint64) []byte {
	if len(dst) == 0 {
		dst = append(dst, 0)
	}
	b0 := uint64(1<<bits - 1)

	if index <= b0 {
		dst[len(dst)-1] |= byte(index)
		return dst
	}

	dst[len(dst)-1] |= byte(b0)
	index -= b0
	for index != 0 {
		dst = append(dst, 128|byte(index&127))
		index >>= 7
	}

	dst[len(dst)-1] &= 127

	return dst
}

// readString reads string from a header field.
// returns the b pointing to the next address, dst and/or error
//
// if error is returned b won't change the pointer address
//
// https://tools.ietf.org/html/rfc7541#section-5.2
func readString(dst, b []byte) ([]byte, []byte, error) {
	var n uint64
	var err error
	mustDecode := b[0]&128 == 128 // huffman encoded
	b, n = readInt(7, b)
	if uint64(len(b)) < n {
		err = fmt.Errorf("unexpected size: %d < %d", len(b), n)
	}
	if err == nil {
		if mustDecode {
			dst = HuffmanDecode(dst, b[:n])
		} else {
			dst = append(dst, b[:n]...)
		}
		b = b[n:]
	}
	return b, dst, err
}

// appendString writes bytes slice to dst and returns it.
// https://tools.ietf.org/html/rfc7541#section-5.2
func appendString(dst, src []byte, encode bool) []byte {
	var b []byte
	if !encode {
		b = src
	} else {
		b = bytePool.Get().([]byte)
		b = HuffmanEncode(b[:0], src)
	}
	// TODO: Encode only if length is lower with the string encoded

	n := uint64(len(b))
	nn := len(dst) - 1 // peek last byte
	if nn >= 0 && dst[nn] != 0 {
		dst = append(dst, 0)
		nn++
	}

	dst = appendInt(dst, 7, n)
	dst = append(dst, b...)

	if encode {
		bytePool.Put(b)
		dst[nn] |= 128 // setting H bit
	}

	return dst
}

// TODO: Change naming
func (hpack *HPACK) AppendHeaderField(h *Headers, hf *HeaderField, store bool) {
	h.rawHeaders = hpack.AppendHeader(h.rawHeaders, hf, store)
}

// AppendHeader appends the content of an encoded HeaderField to dst.
func (hpack *HPACK) AppendHeader(dst []byte, hf *HeaderField, store bool) []byte {
	var (
		c         bool
		bits      uint8
		index     uint64
		fullMatch bool
	)

	c = !hpack.DisableCompression
	bits = 6

	index, fullMatch = hpack.search(hf)
	if hf.sensible {
		c = false
		dst = append(dst, 16)
	} else {
		if index > 0 { // key and/or value can be used as index
			if fullMatch {
				bits, dst = 7, append(dst, indexByte) // can be indexed
			} else if !store { // must be used as literal index
				bits, dst = 4, append(dst, 0)
			} else {
				dst = append(dst, literalByte)
				// append this field to the dynamic table.
				if index < maxIndex {
					hpack.addDynamic(hf)
				}
			}
		} else if !store || hpack.DisableDynamicTable { // with or without indexing
			dst = append(dst, 0, 0)
		} else {
			dst = append(dst, literalByte)
			hpack.addDynamic(hf)
		}
	}

	// the only requirement to write the index is that the idx must be
	// greater than zero. Any Header Field Representation can use indexes.
	if index > 0 {
		dst = appendInt(dst, bits, index)
	} else {
		dst = appendString(dst, hf.key, c)
	}

	// Only writes the value if the prefix is lower than 7. So if the
	// Header Field Representation is not indexed.
	if bits != 7 {
		dst = appendString(dst, hf.value, c)
	}

	return dst
}

var staticTable = []*HeaderField{ // entry + 1
	&HeaderField{key: []byte(":authority")},                          // 1
	&HeaderField{key: []byte(":method"), value: []byte("GET")},       // 2
	&HeaderField{key: []byte(":method"), value: []byte("POST")},      // 3
	&HeaderField{key: []byte(":path"), value: []byte("/")},           // 4
	&HeaderField{key: []byte(":path"), value: []byte("/index.html")}, // 5
	&HeaderField{key: []byte(":scheme"), value: []byte("http")},      // 6
	&HeaderField{key: []byte(":scheme"), value: []byte("https")},     // 7
	&HeaderField{key: []byte(":status"), value: []byte("200")},       // 8
	&HeaderField{key: []byte(":status"), value: []byte("204")},
	&HeaderField{key: []byte(":status"), value: []byte("206")},
	&HeaderField{key: []byte(":status"), value: []byte("304")},
	&HeaderField{key: []byte(":status"), value: []byte("400")},
	&HeaderField{key: []byte(":status"), value: []byte("404")},
	&HeaderField{key: []byte(":status"), value: []byte("500")},
	&HeaderField{key: []byte("accept-charset")},
	&HeaderField{key: []byte("accept-encoding"), value: []byte("gzip, deflate")},
	&HeaderField{key: []byte("accept-language")},
	&HeaderField{key: []byte("accept-ranges")},
	&HeaderField{key: []byte("accept")},
	&HeaderField{key: []byte("access-control-allow-origin")},
	&HeaderField{key: []byte("age")},
	&HeaderField{key: []byte("allow")},
	&HeaderField{key: []byte("authorization")},
	&HeaderField{key: []byte("cache-control")},
	&HeaderField{key: []byte("content-disposition")},
	&HeaderField{key: []byte("content-encoding")},
	&HeaderField{key: []byte("content-language")},
	&HeaderField{key: []byte("content-length")},
	&HeaderField{key: []byte("content-location")},
	&HeaderField{key: []byte("content-range")},
	&HeaderField{key: []byte("content-type")},
	&HeaderField{key: []byte("cookie")},
	&HeaderField{key: []byte("date")},
	&HeaderField{key: []byte("etag")},
	&HeaderField{key: []byte("expect")},
	&HeaderField{key: []byte("expires")},
	&HeaderField{key: []byte("from")},
	&HeaderField{key: []byte("host")},
	&HeaderField{key: []byte("if-match")},
	&HeaderField{key: []byte("if-modified-since")},
	&HeaderField{key: []byte("if-none-match")},
	&HeaderField{key: []byte("if-range")},
	&HeaderField{key: []byte("if-unmodified-since")},
	&HeaderField{key: []byte("last-modified")},
	&HeaderField{key: []byte("link")},
	&HeaderField{key: []byte("location")},
	&HeaderField{key: []byte("max-forwards")},
	&HeaderField{key: []byte("proxy-authenticate")},
	&HeaderField{key: []byte("proxy-authorization")},
	&HeaderField{key: []byte("range")},
	&HeaderField{key: []byte("referer")},
	&HeaderField{key: []byte("refresh")},
	&HeaderField{key: []byte("retry-after")},
	&HeaderField{key: []byte("server")},
	&HeaderField{key: []byte("set-cookie")},
	&HeaderField{key: []byte("strict-transport-security")},
	&HeaderField{key: []byte("transfer-encoding")},
	&HeaderField{key: []byte("user-agent")},
	&HeaderField{key: []byte("vary")},
	&HeaderField{key: []byte("via")},
	&HeaderField{key: []byte("www-authenticate")}, // 61
}

// maxIndex defines the maximum index number of the static table.
const maxIndex = 62
