package http2

import "testing"

// decodeSettings lists the identifiers and values in an encoded payload, so a
// test can assert on what actually goes on the wire rather than on the values
// the struct happens to hold.
func decodeSettings(payload []byte) map[uint16]uint32 {
	out := make(map[uint16]uint32, len(payload)/6)

	for i := 0; i+6 <= len(payload); i += 6 {
		b := payload[i : i+6]
		id := uint16(b[0])<<8 | uint16(b[1])
		out[id] = uint32(b[2])<<24 | uint32(b[3])<<16 | uint32(b[4])<<8 | uint32(b[5])
	}

	return out
}

// TestSettingsEncodePushDisabled covers RFC 7540 6.5.2: SETTINGS_ENABLE_PUSH
// defaults to 1, so an endpoint that wants push off has to say so. Encoding
// only non-zero values means the one value that matters here, 0, is the one
// that never reaches the peer.
func TestSettingsEncodePushDisabled(t *testing.T) {
	var st Settings
	st.Reset()
	st.SetPush(false)
	st.Encode()

	got := decodeSettings(st.rawSettings)

	v, ok := got[EnablePush]
	if !ok {
		t.Fatalf("SETTINGS_ENABLE_PUSH missing from the encoded settings %v", got)
	}

	if v != 0 {
		t.Errorf("SETTINGS_ENABLE_PUSH = %d, want 0", v)
	}
}

// TestSettingsEncodeZeroValue covers the same rule for the other settings whose
// zero is meaningful: HEADER_TABLE_SIZE of 0 turns the dynamic table off and
// MAX_CONCURRENT_STREAMS of 0 refuses new streams.
func TestSettingsEncodeZeroValue(t *testing.T) {
	var st Settings
	st.Reset()
	st.SetHeaderTableSize(0)
	st.SetMaxConcurrentStreams(0)
	st.Encode()

	got := decodeSettings(st.rawSettings)

	for _, id := range []uint16{HeaderTableSize, MaxConcurrentStreams} {
		if _, ok := got[id]; !ok {
			t.Errorf("setting 0x%x missing from the encoded settings %v", id, got)
		}
	}
}

// TestSettingsEncodeOnlyWhatWasSet keeps the encoder honest in the other
// direction: an endpoint that has set nothing has nothing to say, and every
// value it leaves out is the protocol default anyway.
func TestSettingsEncodeOnlyWhatWasSet(t *testing.T) {
	var st Settings
	st.Reset()
	st.SetMaxConcurrentStreams(128)
	st.Encode()

	got := decodeSettings(st.rawSettings)

	if len(got) != 1 {
		t.Fatalf("encoded settings = %v, want only MAX_CONCURRENT_STREAMS", got)
	}

	if got[MaxConcurrentStreams] != 128 {
		t.Errorf("MAX_CONCURRENT_STREAMS = %d, want 128", got[MaxConcurrentStreams])
	}
}

// TestSettingsReadRoundTrip checks that what an endpoint decodes it can encode
// again unchanged, which is what forwarding and copying a peer's settings rely
// on.
func TestSettingsReadRoundTrip(t *testing.T) {
	var src Settings
	src.Reset()
	src.SetPush(false)
	src.SetMaxFrameSize(1 << 15)
	src.Encode()

	var dst Settings
	dst.Reset()

	if err := dst.Read(src.rawSettings); err != nil {
		t.Fatalf("reading the settings: %v", err)
	}

	dst.Encode()

	got := decodeSettings(dst.rawSettings)
	want := decodeSettings(src.rawSettings)

	if len(got) != len(want) {
		t.Fatalf("re-encoded settings = %v, want %v", got, want)
	}

	for id, v := range want {
		if got[id] != v {
			t.Errorf("setting 0x%x = %d, want %d", id, got[id], v)
		}
	}
}

// TestSettingsMergeKeepsAbsentValues covers RFC 7540 6.5: a SETTINGS frame
// carries only the parameters the sender chose to change, and every parameter
// it leaves out keeps the value it already had. Copying the frame over the
// negotiated settings resets the rest to their defaults, which is not what the
// peer said.
func TestSettingsMergeKeepsAbsentValues(t *testing.T) {
	var negotiated Settings
	negotiated.Reset()

	var first Settings
	first.Reset()
	first.SetMaxFrameSize(1 << 15)
	first.SetHeaderTableSize(0)

	first.MergeTo(&negotiated)

	var second Settings
	second.Reset()
	second.SetMaxConcurrentStreams(5)

	second.MergeTo(&negotiated)

	if got := negotiated.MaxFrameSize(); got != 1<<15 {
		t.Errorf("MaxFrameSize = %d, want the %d agreed earlier", got, 1<<15)
	}

	if got := negotiated.HeaderTableSize(); got != 0 {
		t.Errorf("HeaderTableSize = %d, want the 0 agreed earlier", got)
	}

	if got := negotiated.MaxConcurrentStreams(); got != 5 {
		t.Errorf("MaxConcurrentStreams = %d, want 5", got)
	}
}

// TestSettingsMergeLeavesUntouchedDefaults checks the other half: a parameter
// neither side has ever sent still reads as its protocol default.
func TestSettingsMergeLeavesUntouchedDefaults(t *testing.T) {
	var negotiated Settings
	negotiated.Reset()

	var st Settings
	st.Reset()
	st.SetMaxConcurrentStreams(5)

	st.MergeTo(&negotiated)

	if got := negotiated.MaxFrameSize(); got != defaultDataFrameSize {
		t.Errorf("MaxFrameSize = %d, want the default %d", got, defaultDataFrameSize)
	}
}
