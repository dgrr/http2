package http2

import (
	"bytes"
	"errors"
	"fmt"
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

// Size returns the header field size as RFC specifies.
//
// https://tools.ietf.org/html/rfc7541#section-4.1
func (hf *HeaderField) Size() int {
	return len(hf.name) + len(hf.value) + 32
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

// HPACK represents header compression methods to
// encode and decode header fields in HTTP/2.
//
// HPACK is the same as HTTP/1.1 header.
//
// Use AcquireHPACK to acquire new HPACK structure
// TODO: HPACK to Headers?
type HPACK struct {
	noCopy noCopy

	// DisableCompression disables compression for literal header fields.
	DisableCompression bool

	// fields are the header fields
	fields []*HeaderField

	// dynamic represents the dynamic table
	dynamic []*HeaderField

	tableSize    int
	maxTableSize int
}

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPACK{
			maxTableSize: int(defaultHeaderTableSize),
		}
	},
}

// AcquireHPACK gets HPACK from pool
func AcquireHPACK() *HPACK {
	return hpackPool.Get().(*HPACK)
}

// ReleaseHPACK puts HPACK to the pool
func ReleaseHPACK(hpack *HPACK) {
	hpack.Reset()
	hpackPool.Put(hpack)
}

func (hpack *HPACK) releaseTable() {
	for _, hf := range hpack.dynamic {
		ReleaseHeaderField(hf)
	}
	hpack.dynamic = hpack.dynamic[:0]
}

func (hpack *HPACK) releaseFields() {
	for _, hf := range hpack.fields {
		ReleaseHeaderField(hf)
	}
	hpack.fields = hpack.fields[:0]
}

// Reset deletes and realeases all dynamic header fields
func (hpack *HPACK) Reset() {
	hpack.releaseTable()
	hpack.releaseFields()
	hpack.tableSize = 0
	hpack.maxTableSize = int(defaultHeaderTableSize)
	hpack.DisableCompression = false
}

// Add adds the name and the value to the Header.
func (hpack *HPACK) Add(name, value string) {
	hf := AcquireHeaderField()
	hf.SetName(name)
	hf.SetValue(value)
	hpack.fields = append(hpack.fields, hf)
}

// ....
func (hpack *HPACK) AddBytes(name, value []byte) {
	hpack.Add(b2s(name), b2s(value))
}

// ...
func (hpack *HPACK) AddBytesK(name []byte, value string) {
	hpack.Add(b2s(name), value)
}

// ...
func (hpack *HPACK) AddBytesV(name string, value []byte) {
	hpack.Add(name, b2s(value))
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
func (hpack *HPACK) add(hf *HeaderField) {
	// TODO: https://tools.ietf.org/html/rfc7541#section-2.3.2
	// apply duplicate entries
	// TODO: Optimize using reverse indexes.
	mustAdd := true

	i := 0
	for i = range hpack.dynamic {
		// searching if the HeaderField already exist.
		hf2 := hpack.dynamic[i]
		if bytes.Equal(hf.name, hf2.name) {
			// if exist update the value.
			mustAdd = false
			hf2.SetValueBytes(hf.value)
			break
		}
	}

	if !mustAdd {
		hf = hpack.dynamic[i]
		// moving the HeaderField to the first element.
		for i > 0 {
			hpack.dynamic[i] = hpack.dynamic[i-1]
			i--
		}
		hpack.dynamic[0] = hf
		hpack.shrink(0)
	} else {
		// checking table size
		hpack.shrink(hf.Size())

		hf2 := AcquireHeaderField()
		hf.CopyTo(hf2)
		if len(hpack.dynamic) == 0 {
			hpack.dynamic = append(hpack.dynamic, hf2)
		} else {
			hpack.dynamic = append(hpack.dynamic[:1], hpack.dynamic...)
			hpack.dynamic[0] = hf2
		}
	}
}

// shrink shrinks the dynamic table if needed.
func (hpack *HPACK) shrink(add int) {
	for {
		hpack.tableSize = hpack.DynamicSize() + add
		if hpack.tableSize <= hpack.maxTableSize {
			break
		}
		n := len(hpack.dynamic) - 1
		if n == -1 {
			break // TODO: panic()?
		}
		// release the header field
		ReleaseHeaderField(hpack.dynamic[n])
		// shrinking slice
		hpack.dynamic = hpack.dynamic[:n]
	}
}

// Peek returns HeaderField value of the given name.
//
// value will be nil if name is not found.
func (hpack *HPACK) Peek(name string) (value []byte) {
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
func (hpack *HPACK) PeekBytes(name []byte) (value []byte) {
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
func (hpack *HPACK) PeekField(name string) (hf *HeaderField) {
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
func (hpack *HPACK) PeekFieldBytes(name []byte) (hf *HeaderField) {
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
func (hpack *HPACK) peek(n uint64) (hf *HeaderField) {
	// TODO: Change peek function name
	if n < maxIndex {
		hf = staticTable[n-1]
	} else { // search in dynamic table
		nn := int(n - maxIndex)
		if nn < len(hpack.dynamic) {
			hf = hpack.dynamic[nn]
		}
	}
	return
}

// find gets the index of existent name in static or dynamic tables.
func (hpack *HPACK) search(hf *HeaderField) (n uint64) {
	// start searching in the dynamic table (probably it contains less fields than the static.
	for i, hf2 := range hpack.dynamic {
		if bytes.Equal(hf.name, hf2.name) && bytes.Equal(hf.value, hf2.value) {
			n = uint64(i + maxIndex)
			break
		}
	}
	if n == 0 {
		for i, hf2 := range staticTable {
			if bytes.Equal(hf.name, hf2.name) {
				if n != 0 {
					if bytes.Equal(hf.value, hf2.value) {
						// must add 1 because of indexing
						n = uint64(i + 1)
						break
					}
				} else {
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

// Read reads header fields from b and stores in hpack.
//
// The returned values are the new b pointing to the next data to be read and/or error.
//
// This function must receive the payload of Header frame.
func (hpack *HPACK) Read(b []byte) ([]byte, error) {
	// TODO: Change Read to Write?
	var (
		n   uint64
		c   byte
		err error
		hf  *HeaderField
	)
	for len(b) > 0 {
		c = b[0]
		switch {
		// Indexed Header Field.
		// The value must be indexed in the static or the dynamic table.
		// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
		case c&indexByte == indexByte: // 1000 0000
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
		case c&literalByte == literalByte: // 0100 0000
			// Reading name
			if c != 64 { // Read name as index
				b, n, err = readInt(6, b)
				if err == nil {
					if hf = hpack.peek(n); hf != nil {
						if n < maxIndex { // peek from static table. MUST not be modified
							hf2 := AcquireHeaderField()
							hf.CopyTo(hf2)
							hf = hf2
						}
					} else {
						// TODO: error
						panic("error")
					}
				}
			} else { // Read name literal string
				// Huffman encoded or not
				b = b[1:]
				dst := bytePool.Get().([]byte)
				b, dst, err = readString(dst[:0], b)
				if err == nil {
					hf = AcquireHeaderField()
					hf.SetNameBytes(dst)
				}
				bytePool.Put(dst)
			}
			// Reading value
			if err == nil {
				if b[0] == c {
					b = b[1:]
				}
				dst := bytePool.Get().([]byte)
				if err == nil {
					b, dst, err = readString(dst[:0], b)
					if err == nil {
						hf.SetValueBytes(dst)
					}
					// add to the table as RFC specifies.
					hpack.add(hf)
				}
				bytePool.Put(dst)
			}

		// Literal Header Field Never Indexed.
		// The value of this field must not be encoded
		// https://tools.ietf.org/html/rfc7541#section-6.2.3
		case c&noIndexByte == 16: // 0001 0000
			hf = AcquireHeaderField()
			hf.sensible = true
			fallthrough
		// Header Field without Indexing.
		// This header field must not be appended to the dynamic table.
		// https://tools.ietf.org/html/rfc7541#section-6.2.2
		case c&noIndexByte == 0: // 0000 0000
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
				b = b[1:]
				dst := bytePool.Get().([]byte)
				b, dst, err = readString(dst[:0], b)
				if err == nil {
					hf.SetNameBytes(dst)
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
			b, n, err = readInt(5, b)
			if err == nil {
				hpack.maxTableSize = int(n)
			}
		}

		if err != nil {
			if hf != nil {
				ReleaseHeaderField(hf)
				// hf = nil
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

// appendInt appends int type to header field excluding the last byte
// which will be OR'ed.
// https://tools.ietf.org/html/rfc7541#section-5.1
func appendInt(dst []byte, n uint8, nn uint64) []byte {
	nu := uint64(1<<n - 1)
	m := len(dst)
	if m == 0 {
		dst = append(dst, 0)
		m++
	}

	if nn < nu {
		dst[m-1] |= byte(nn)
	} else {
		nn -= nu
		dst[m-1] |= byte(nu)
		m = len(dst)
		nu = 1 << (n + 1)
		i := 0
		for nn > 0 {
			i++
			if i == m {
				dst = append(dst, 0)
				m++
			}
			dst[i] = byte(nn | 128)
			nn >>= 7
		}
		dst[i] &= 127
	}
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
	mustDecode := (b[0]&128 == 128) // huffman encoded
	b, n, err = readInt(7, b)
	if err == nil && uint64(len(b)) < n {
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
	nn := len(dst) // peek first byte
	if nn > 0 && dst[nn-1] != 0 {
		dst = append(dst, 0)
	}
	dst = appendInt(dst, 7, n)
	dst = append(dst, b...)

	if encode {
		bytePool.Put(b)
		dst[nn] |= 128 // setting H bit
	}
	return dst
}

var errHeaderFieldNotFound = errors.New("Indexed field not found")

// Write writes hpack to dst returning the result byte slice.
func (hpack *HPACK) Write(dst []byte) ([]byte, error) {
	var c bool
	var n uint8
	var idx uint64
	for _, hf := range hpack.fields {
		c = !hpack.DisableCompression
		n = 4

		idx = hpack.search(hf)
		if hf.sensible {
			c = false
			dst = append(dst, 16)
		} else {
			if idx > 0 { // name and/or value can be used as index
				hf2 := hpack.peek(idx)
				if idx > maxIndex || bytes.Equal(hf.value, hf2.value) {
					n, dst = 7, append(dst, indexByte) // could be indexed
				} else { // must be used as literal index
					n, dst = 6, append(dst, literalByte)
					// append this field to the dynamic table.
					hpack.add(hf)
				}
			} else { // with or without indexing
				// if is client run this code
				n, dst = 6, append(dst, literalByte)
				hpack.add(hf)
				// if not run this
				//dst = append(dst, 0, 0) // without indexing
				// TODO: use the dynamic table only in the client side.
			}
		}

		// the only requirement to write the index is that the idx must be
		// granther than zero. Any Header Field Representation can use indexes.
		if idx > 0 {
			dst = appendInt(dst, n, idx)
		} else {
			dst = appendString(dst, hf.name, c)
		}
		// Only writes the value if the prefix is lower than 7. So if the
		// Header Field Representation is not indexed.
		if n != 7 {
			dst = appendString(dst, hf.value, c)
		}
	}
	return dst, nil
}

// TODO: Change to non-pointer?
var staticTable = []*HeaderField{ // entry + 1
	&HeaderField{name: []byte(":authority")},                          // 1
	&HeaderField{name: []byte(":method"), value: []byte("GET")},       // 2
	&HeaderField{name: []byte(":method"), value: []byte("POST")},      // 3
	&HeaderField{name: []byte(":path"), value: []byte("/")},           // 4
	&HeaderField{name: []byte(":path"), value: []byte("/index.html")}, // 5
	&HeaderField{name: []byte(":scheme"), value: []byte("http")},      // 6
	&HeaderField{name: []byte(":scheme"), value: []byte("https")},     // 7
	&HeaderField{name: []byte(":status"), value: []byte("200")},       // 8
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
const maxIndex = 62
