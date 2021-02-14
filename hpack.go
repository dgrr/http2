package http2

import (
	"bytes"
	"errors"
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

	// fields are the header fields
	// TODO: Replace HeaderField with a argsKV
	fields []*HeaderField

	// dynamic represents the dynamic table
	dynamic []*HeaderField

	tableSize    int
	maxTableSize int
}

// func (hp *HPACK) Fork() *HPACK {
// 	hp2 := AcquireHPACK()
// 	for i := range hp.dynamic {
// 		hf := AcquireHeaderField()
// 		hp.dynamic[i].CopyTo(hf)
// 		hp2.dynamic = append(hp2.dynamic, hf)
// 	}
//
// 	return hp2
// }

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPACK{
			maxTableSize: int(defaultHeaderTableSize),
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

func (hpack *HPACK) releaseFields() {
	for _, hf := range hpack.fields {
		ReleaseHeaderField(hf)
	}
	hpack.fields = hpack.fields[:0]
}

// Reset deletes and realeases all dynamic header fields
func (hpack *HPACK) Reset() {
	hpack.releaseDynamic()
	hpack.releaseFields()
	hpack.tableSize = 0
	hpack.maxTableSize = int(defaultHeaderTableSize)
	hpack.DisableCompression = false
}

// AppendBytes appends hpack headers to dst and returns the new dst.
func (hpack *HPACK) AppendBytes(dst []byte) []byte {
	for _, hf := range hpack.fields {
		dst = hf.AppendBytes(dst)
		dst = append(dst, '\n')
	}
	return dst
}

// String returns HPACK as a string like an HTTP/1.1 header.
func (hpack *HPACK) String() string {
	return string(hpack.AppendBytes(nil))
}

func (hpack *HPACK) Range(fn func(*HeaderField)) {
	for i := range hpack.fields {
		fn(hpack.fields[i])
	}
}

// AddField appends the field to the header list.
func (hpack *HPACK) AddField(hf *HeaderField) {
	hpack.fields = append(hpack.fields, hf)
}

// Add adds the key and the value to the Header.
func (hpack *HPACK) Add(key, value string) {
	hf := AcquireHeaderField()
	hf.SetKey(key)
	hf.SetValue(value)
	hpack.AddField(hf)
}

// ....
func (hpack *HPACK) AddBytes(key, value []byte) {
	hpack.Add(b2s(key), b2s(value))
}

// ...
func (hpack *HPACK) AddBytesK(key []byte, value string) {
	hpack.Add(b2s(key), value)
}

// ...
func (hpack *HPACK) AddBytesV(key string, value []byte) {
	hpack.Add(key, b2s(value))
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
	// TODO: https://tools.ietf.org/html/rfc7541#section-2.3.2
	// apply duplicate entries
	// TODO: Optimize using reverse indexes.
	mustAdd := true

	i := 0
	for i = range hpack.dynamic {
		// searching if the HeaderField already exist.
		hf2 := hpack.dynamic[i]
		if bytes.Equal(hf.key, hf2.key) {
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

		// append a copy
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

// Peek returns HeaderField value of the given key.
//
// value will be nil if key is not found.
func (hpack *HPACK) Peek(key string) (value []byte) {
	for _, hf := range hpack.fields {
		if b2s(hf.key) == key {
			value = hf.value
			break
		}
	}
	return
}

// PeekBytes returns HeaderField value of the given key in bytes.
//
// value will be nil if key is not found.
func (hpack *HPACK) PeekBytes(key []byte) (value []byte) {
	for _, hf := range hpack.fields {
		if bytes.Equal(hf.key, key) {
			value = hf.value
			break
		}
	}
	return
}

// PeekField returns HeaderField structure of the given key.
//
// hf will be nil in case key is not found.
func (hpack *HPACK) PeekField(key string) (hf *HeaderField) {
	// TODO: hf must be a copy or pointer?
	for _, hf2 := range hpack.fields {
		if b2s(hf2.key) == key {
			hf = hf2
			break
		}
	}
	return
}

// PeekFieldBytes returns HeaderField structure of the given key in bytes.
//
// hf will be nil in case key is not found.
func (hpack *HPACK) PeekFieldBytes(key []byte) (hf *HeaderField) {
	// TODO: hf must be a copy or pointer?
	for _, hf2 := range hpack.fields {
		if bytes.Equal(hf2.key, key) {
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
	// TODO: Change peek function key
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

// find gets the index of existent key in static or dynamic tables.
func (hpack *HPACK) search(hf *HeaderField) (n uint64) {
	// start searching in the dynamic table (probably it contains less fields than the static.
	for i, hf2 := range hpack.dynamic {
		if bytes.Equal(hf.key, hf2.key) && bytes.Equal(hf.value, hf2.value) {
			n = uint64(i + maxIndex)
			break
		}
	}
	if n == 0 {
		for i, hf2 := range staticTable {
			if bytes.Equal(hf.key, hf2.key) {
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

	c = b[0]
	switch {
	// Indexed Header Field.
	// The value must be indexed in the static or the dynamic table.
	// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
	case c&indexByte == indexByte: // 1000 0000
		b, n, err = readInt(7, b)
		if err == nil {
			if hf2 := hpack.peek(n); hf2 != nil {
				hf2.CopyTo(hf)
			}
		}

	// Literal Header Field with Incremental Indexing.
	// Key can be indexed or not. So if the first byte is equal to 64
	// the key value must be appended to the dynamic table.
	// https://tools.ietf.org/html/rfc7541#section-6.1
	case c&literalByte == literalByte: // 0100 0000
		// Reading key
		if c != 64 { // Read key as index
			b, n, err = readInt(6, b)
			if err == nil {
				if hf2 := hpack.peek(n); hf2 != nil {
					if n < maxIndex { // peek from static table. MUST not be modified
						hf2.CopyTo(hf)
					}
				} else {
					// TODO: error
					panic("error")
				}
			}
		} else { // Read key literal string
			// Huffman encoded or not
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
			if err == nil {
				b, dst, err = readString(dst[:0], b)
				if err == nil {
					hf.SetValueBytes(dst)
				}
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
			b, n, err = readInt(4, b)
			if err == nil {
				if hf2 := hpack.peek(n); hf2 != nil {
					hf.SetKeyBytes(hf2.key)
				} else {
					// TODO: error
				}
			}
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
		b, n, err = readInt(5, b)
		if err == nil {
			hpack.maxTableSize = int(n)
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
			return b[i:], 0, errors.New("bit overflow reading an int")
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
	m := len(dst) - 1
	if m == -1 {
		dst = append(dst, 0)
		m++
	}

	if nn < nu {
		dst[m] |= byte(nn)
	} else {
		nn -= nu
		dst[m] |= byte(nu)
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
	mustDecode := b[0]&128 == 128 // huffman encoded
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

var errHeaderFieldNotFound = errors.New("indexed field not found")

func (hpack *HPACK) MarshalTo(dst []byte) []byte {
	for i := range hpack.fields {
		dst = hpack.AppendHeader(dst, hpack.fields[i])
	}
	return dst
}

// AppendHeader writes hpack to dst returning the result byte slice.
func (hpack *HPACK) AppendHeader(dst []byte, hf *HeaderField) []byte {
	var c bool
	var n uint8
	var idx uint64

	c = !hpack.DisableCompression
	n = 6

	idx = hpack.search(hf)
	if hf.sensible {
		c = false
		dst = append(dst, 16)
	} else {
		if idx > 0 { // key and/or value can be used as index
			hf2 := hpack.peek(idx)
			if bytes.Equal(hf.value, hf2.value) {
				n, dst = 7, append(dst, indexByte) // could be indexed
			} else if hpack.DisableDynamicTable { // must be used as literal index
				n, dst = 4, append(dst, 0)
			} else {
				dst = append(dst, literalByte)
				// append this field to the dynamic table.
				hpack.addDynamic(hf)
			}
		} else if hpack.DisableDynamicTable { // with or without indexing
			dst = append(dst, 0, 0)
		} else {
			dst = append(dst, literalByte)
			hpack.addDynamic(hf)
		}
	}

	// the only requirement to write the index is that the idx must be
	// granther than zero. Any Header Field Representation can use indexes.
	if idx > 0 {
		dst = appendInt(dst, n, idx)
	} else {
		dst = appendString(dst, hf.key, c)
	}
	// Only writes the value if the prefix is lower than 7. So if the
	// Header Field Representation is not indexed.
	if n != 7 {
		dst = appendString(dst, hf.value, c)
	}

	return dst
}

// TODO: Change to non-pointer?
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
