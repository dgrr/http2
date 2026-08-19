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
// HPACK is equivalent to an HTTP/1 header.
//
// Use AcquireHPACK to acquire new HPACK structure.
type HPACK struct {
	// DisableCompression disables compression for literal header fields.
	DisableCompression bool

	// DisableDynamicTable disables the usage of the dynamic table for
	// the HPACK structure. If this option is true the HPACK won't add any
	// field to the dynamic table unless it was sent by the peer.
	//
	// This field was implemented because in many ways the server could modify
	// the fields established by the client losing performance calculated by client.
	DisableDynamicTable bool

	// the dynamic table is in an inverse order.
	//
	// the insertion point should be the beginning. But we are going to do
	// the opposite, insert on the end and drop on the beginning.
	//
	// To get the original index then we need to do the following:
	// dynamic_length - (input_index - 62) - 1
	dynamic []*HeaderField

	maxTableSize uint32
	// maxTableSize coming from the settings frame
	maxTableSizeSettings uint32

	// pendingSizeUpdate is set when the maximum table size changed and the peer
	// has not been told yet. Shrinking the table without saying so leaves the
	// peer's decoder with entries we no longer have, which shows up later as a
	// COMPRESSION_ERROR on a header that indexed one of them.
	// https://tools.ietf.org/html/rfc7541#section-6.3
	pendingSizeUpdate bool
}

func headerFieldsToString(hfs []*HeaderField, indexOffset int) string {
	s := ""

	for i := len(hfs) - 1; i >= 0; i-- {
		s += fmt.Sprintf("%d - %s\n", (len(hfs)-i)+indexOffset-1, hfs[i])
	}

	return s
}

var hpackPool = sync.Pool{
	New: func() interface{} {
		return &HPACK{
			maxTableSize:         defaultHeaderTableSize,
			maxTableSizeSettings: defaultHeaderTableSize,
			dynamic:              make([]*HeaderField, 0, 16),
		}
	},
}

// AcquireHPACK gets HPACK from pool.
func AcquireHPACK() *HPACK {
	// TODO: Change the name
	hp := hpackPool.Get().(*HPACK)
	hp.Reset()

	return hp
}

// ReleaseHPACK puts HPACK to the pool.
func ReleaseHPACK(hp *HPACK) {
	hpackPool.Put(hp)
}

func (hp *HPACK) releaseDynamic() {
	for _, hf := range hp.dynamic {
		ReleaseHeaderField(hf)
	}

	hp.dynamic = hp.dynamic[:0]
}

// Reset deletes and releases all dynamic header fields.
func (hp *HPACK) Reset() {
	hp.releaseDynamic()
	hp.maxTableSize = defaultHeaderTableSize
	hp.maxTableSizeSettings = defaultHeaderTableSize
	hp.pendingSizeUpdate = false
	hp.DisableCompression = false
}

// SetMaxTableSize sets the maximum dynamic table size.
//
// On an encoder the change is announced to the peer at the start of the next
// header block, as the RFC requires.
func (hp *HPACK) SetMaxTableSize(size uint32) {
	if hp.maxTableSize == size && hp.maxTableSizeSettings == size {
		return
	}

	hp.maxTableSizeSettings = size
	hp.maxTableSize = size
	hp.pendingSizeUpdate = true

	hp.shrink()
}

// DynamicSize returns the size of the dynamic table.
//
// https://tools.ietf.org/html/rfc7541#section-4.1
func (hp *HPACK) DynamicSize() (n uint32) {
	for _, hf := range hp.dynamic {
		n += hf.Size()
	}
	return n
}

// add header field to the dynamic table.
func (hp *HPACK) addDynamic(hf *HeaderField) {
	// TODO: Optimize using reverse indexes.

	// append a copy
	hf2 := AcquireHeaderField()
	hf.CopyTo(hf2)

	hp.dynamic = append(hp.dynamic, hf2)

	// checking table size
	hp.shrink()
}

// shrink the dynamic table if needed.
func (hp *HPACK) shrink() {
	var n int // elements to remove
	tableSize := hp.DynamicSize()

	for n = 0; n < len(hp.dynamic) && tableSize > hp.maxTableSize; n++ {
		tableSize -= hp.dynamic[n].Size()
	}

	if n != 0 {
		for i := 0; i < n; i++ {
			// release the header field
			ReleaseHeaderField(hp.dynamic[i])
			// shrinking slice
		}

		hp.dynamic = append(hp.dynamic[:0], hp.dynamic[n:]...)
	}
}

// peek returns HeaderField from static or dynamic table.
//
// n must be the index in the table.
func (hp *HPACK) peek(n uint64) *HeaderField {
	var (
		index int
		table []*HeaderField
	)

	if n < maxIndex {
		index, table = int(n-1), staticTable
	} else { // search in dynamic table
		nn := len(hp.dynamic) - int(n-maxIndex) - 1
		// dynamic_len = 11
		// n = 64
		// nn = 11 - (64 - 62) - 1 = 8
		index, table = nn, hp.dynamic
	}

	// n comes off the wire, so the computed index can land anywhere. Callers
	// turn a nil result into a decoding error.
	if index < 0 || index >= len(table) {
		return nil
	}

	return table[index]
}

// find gets the index of existent key in static or dynamic tables.
func (hp *HPACK) search(hf *HeaderField) (n uint64, fullMatch bool) {
	// start searching in the dynamic table (probably it contains fewer fields than the static).
	for i, hf2 := range hp.dynamic {
		if fullMatch = bytes.Equal(hf.key, hf2.key) && bytes.Equal(hf.value, hf2.value); fullMatch {
			n = uint64(maxIndex + len(hp.dynamic) - i - 1)
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

	return n, fullMatch
}

const (
	indexByte   = 128 // 10000000
	literalByte = 64  // 01000000
	noIndexByte = 240 // 11110000
)

// bytePool holds scratch buffers for string coding. It stores pointers rather
// than slices: putting a slice back boxes it into an interface, which is an
// allocation on every header field.
var bytePool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 128)
		return &b
	},
}

func acquireScratch() *[]byte {
	return bytePool.Get().(*[]byte)
}

func releaseScratch(b *[]byte) {
	bytePool.Put(b)
}

// Next reads and process the contents of `b`. If `b` contains a valid HTTP/2 header
// the content will be parsed into `hf`.
//
// This function returns the next byte slice that should be read.
// `b` must be a valid payload coming from a Header frame.
func (hp *HPACK) Next(hf *HeaderField, b []byte) ([]byte, error) {
	return hp.nextField(hf, 0, 0, b)
}

func (hp *HPACK) nextField(hf *HeaderField, headerBlockNum, fieldsProcessed int, b []byte) ([]byte, error) {
	var (
		n   uint64
		c   byte
		err error
	)

loop:
	if len(b) == 0 {
		return b, nil
	}

	c = b[0]

	switch {
	// Indexed Header Field.
	// The value must be indexed in the static or the dynamic table.
	// https://httpwg.org/specs/rfc7541.html#indexed.header.representation
	case c&indexByte == indexByte: // 1000 0000
		if b, n, err = readInt(7, b); err != nil {
			return b, err
		}

		hf2 := hp.peek(n)
		if hf2 == nil {
			return b, NewError(FlowControlError, fmt.Sprintf("index field not found: %d. table:\n%s", n,
				headerFieldsToString(hp.dynamic, maxIndex)))
		}

		hf2.CopyTo(hf)

	// Literal Header Field with Incremental Indexing.
	// Key can be indexed or not. Then appended to the table
	// https://tools.ietf.org/html/rfc7541#section-6.1
	case c&literalByte == literalByte: // 0100 0000
		// Reading key
		if c != 64 { // Read key as index
			if b, n, err = readInt(6, b); err != nil {
				return b, err
			}

			hf2 := hp.peek(n)
			if hf2 == nil {
				return b, NewError(FlowControlError, fmt.Sprintf("literal indexed field not found: %d. table:\n%s",
					n, headerFieldsToString(hp.dynamic, maxIndex)))
			}

			hf.SetKeyBytes(hf2.KeyBytes())
		} else { // Read key literal string
			b = b[1:]
			scratch := acquireScratch()
			dst := *scratch

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetKeyBytes(dst)
			}

			*scratch = dst
			releaseScratch(scratch)
		}

		// Reading value
		if err == nil {
			if len(b) == 0 {
				// The field is cut short: its value is in the bytes that have
				// not arrived yet.
				return b, ErrUnexpectedSize
			}

			if b[0] == c {
				b = b[1:]
			}

			scratch := acquireScratch()
			dst := *scratch

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetValueBytes(dst)
				// add to the table as RFC specifies.
				hp.addDynamic(hf)
			}

			*scratch = dst
			releaseScratch(scratch)
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
		if c&15 != 0 { // Reading key as index
			if b, n, err = readInt(4, b); err != nil {
				return b, err
			}

			hf2 := hp.peek(n)
			if hf2 == nil {
				return b, NewError(FlowControlError, fmt.Sprintf("non indexed field not found: %d. table:\n%s", n,
					headerFieldsToString(hp.dynamic, maxIndex)))
			}

			hf.SetKeyBytes(hf2.key)
		} else { // Reading key as string literal
			b = b[1:]
			scratch := acquireScratch()
			dst := *scratch

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetKeyBytes(dst)
			}

			*scratch = dst
			releaseScratch(scratch)
		}

		// Reading value
		if err == nil {
			if len(b) == 0 {
				// The field is cut short: its value is in the bytes that have
				// not arrived yet.
				return b, ErrUnexpectedSize
			}

			if b[0] == c {
				b = b[1:]
			}

			scratch := acquireScratch()
			dst := *scratch

			b, dst, err = readString(dst[:0], b)
			if err == nil {
				hf.SetValueBytes(dst)
			}

			*scratch = dst
			releaseScratch(scratch)
		}

	// Dynamic Table Size Update
	// Changes the size of the dynamic table.
	// https://tools.ietf.org/html/rfc7541#section-6.3
	case c&32 == 32: // 001- ----
		if b, n, err = readInt(5, b); err != nil {
			return b, err
		}
		// Dynamic table size
		// update MUST occur at the beginning of the first header block
		// following the change to the dynamic table size.
		if headerBlockNum != 0 || fieldsProcessed > 0 {
			return nil, ErrDynamicUpdate
		}

		if n > uint64(hp.maxTableSizeSettings) {
			return nil, ErrDynamicUpdateMaxTableSize
		}

		hp.maxTableSize = uint32(n)
		hp.shrink()

		goto loop
	}

	return b, err
}

// readInt reads int type from header field.
//
// A varint whose continuation bit is still set when the bytes run out returns
// ErrUnexpectedSize, which the caller reads as "the rest is in a CONTINUATION
// frame" rather than as a decoding failure. One that cannot fit in a uint64 is
// a decoding failure and returns ErrIntOverflow.
// https://tools.ietf.org/html/rfc7541#section-5.1
func readInt(n int, b []byte) ([]byte, uint64, error) {
	if len(b) == 0 {
		return b, 0, ErrUnexpectedSize
	}

	// 1<<7 - 1 = 0111 1111
	b0 := byte(1<<n - 1)
	// if b[0] = 0111 1111 then continue reading the int
	// if not, then we are done
	// if b0 is 0011 1111, then b0&b[0] != b0 = false
	if b0&b[0] != b0 {
		return b[1:], uint64(b[0] & b0), nil
	}

	nn := uint64(0)

	for i := 1; i < len(b); i++ {
		if shift := (i - 1) * 7; shift >= 64 {
			return b, 0, ErrIntOverflow
		} else {
			nn |= uint64(b[i]&127) << shift
		}

		if b[i]&128 != 128 {
			return b[i+1:], nn + uint64(b0), nil
		}
	}

	// The bytes ran out with the continuation bit still set.
	return b, 0, ErrUnexpectedSize
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

	if len(b) == 0 {
		return b, dst, errors.New("no bytes left reading a string. Malformed data?")
	}

	mustDecode := b[0]&128 == 128 // huffman encoded

	b, n, err := readInt(7, b)
	if err != nil {
		return b, dst, err
	}

	if uint64(len(b)) < n {
		return b, dst, ErrUnexpectedSize
	}

	if mustDecode {
		dst, err = HuffmanDecode(dst, b[:n])
	} else {
		dst = append(dst, b[:n]...)
	}

	if err != nil {
		return b, nil, err
	}

	b = b[n:]

	return b, dst, nil
}

var (
	ErrUnexpectedSize            = errors.New("unexpected size")
	ErrIntOverflow               = errors.New("integer in the header block does not fit in 64 bits")
	ErrDynamicUpdate             = errors.New("dynamic update received after the first header block")
	ErrDynamicUpdateMaxTableSize = errors.New("dynamic update is over the max table")
)

// appendString writes bytes slice to dst and returns it.
// https://tools.ietf.org/html/rfc7541#section-5.2
func appendString(dst, src []byte, encode bool) []byte {
	var (
		b       []byte
		scratch *[]byte
	)

	if !encode {
		b = src
	} else {
		scratch = acquireScratch()
		b = HuffmanEncode((*scratch)[:0], src)
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
		*scratch = b

		releaseScratch(scratch)

		dst[nn] |= 128 // setting H bit
	}

	return dst
}

// TODO: Change naming.
func (hp *HPACK) AppendHeaderField(h *Headers, hf *HeaderField, store bool) {
	h.rawHeaders = hp.AppendHeader(h.rawHeaders, hf, store)
}

// AppendHeader appends the content of an encoded HeaderField to dst.
func (hp *HPACK) AppendHeader(dst []byte, hf *HeaderField, store bool) []byte {
	var (
		c         bool
		bits      uint8
		index     uint64
		fullMatch bool
	)

	// A dynamic table size update has to be the first thing in the block that
	// follows the change.
	if hp.pendingSizeUpdate {
		hp.pendingSizeUpdate = false

		dst = appendInt(append(dst, 0x20), 5, uint64(hp.maxTableSize))
	}

	c = !hp.DisableCompression
	bits = 6

	index, fullMatch = hp.search(hf)
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
					hp.addDynamic(hf)
				}
			}
		} else if !store || hp.DisableDynamicTable { // with or without indexing
			dst = append(dst, 0, 0)
		} else {
			dst = append(dst, literalByte)
			hp.addDynamic(hf)
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
	{key: []byte(":authority")},                          // 1
	{key: []byte(":method"), value: []byte("GET")},       // 2
	{key: []byte(":method"), value: []byte("POST")},      // 3
	{key: []byte(":path"), value: []byte("/")},           // 4
	{key: []byte(":path"), value: []byte("/index.html")}, // 5
	{key: []byte(":scheme"), value: []byte("http")},      // 6
	{key: []byte(":scheme"), value: []byte("https")},     // 7
	{key: []byte(":status"), value: []byte("200")},       // 8
	{key: []byte(":status"), value: []byte("204")},
	{key: []byte(":status"), value: []byte("206")},
	{key: []byte(":status"), value: []byte("304")},
	{key: []byte(":status"), value: []byte("400")},
	{key: []byte(":status"), value: []byte("404")},
	{key: []byte(":status"), value: []byte("500")},
	{key: []byte("accept-charset")},
	{key: []byte("accept-encoding"), value: []byte("gzip, deflate")},
	{key: []byte("accept-language")},
	{key: []byte("accept-ranges")},
	{key: []byte("accept")},
	{key: []byte("access-control-allow-origin")},
	{key: []byte("age")},
	{key: []byte("allow")},
	{key: []byte("authorization")},
	{key: []byte("cache-control")},
	{key: []byte("content-disposition")},
	{key: []byte("content-encoding")},
	{key: []byte("content-language")},
	{key: []byte("content-length")},
	{key: []byte("content-location")},
	{key: []byte("content-range")},
	{key: []byte("content-type")},
	{key: []byte("cookie")},
	{key: []byte("date")},
	{key: []byte("etag")},
	{key: []byte("expect")},
	{key: []byte("expires")},
	{key: []byte("from")},
	{key: []byte("host")},
	{key: []byte("if-match")},
	{key: []byte("if-modified-since")},
	{key: []byte("if-none-match")},
	{key: []byte("if-range")},
	{key: []byte("if-unmodified-since")},
	{key: []byte("last-modified")},
	{key: []byte("link")},
	{key: []byte("location")},
	{key: []byte("max-forwards")},
	{key: []byte("proxy-authenticate")},
	{key: []byte("proxy-authorization")},
	{key: []byte("range")},
	{key: []byte("referer")},
	{key: []byte("refresh")},
	{key: []byte("retry-after")},
	{key: []byte("server")},
	{key: []byte("set-cookie")},
	{key: []byte("strict-transport-security")},
	{key: []byte("transfer-encoding")},
	{key: []byte("user-agent")},
	{key: []byte("vary")},
	{key: []byte("via")},
	{key: []byte("www-authenticate")}, // 61
}

// maxIndex defines the maximum index number of the static table.
const maxIndex = 62
