package http2

// func TestClientWriteOrder(t *testing.T) {
// bf := bytes.NewBuffer(nil)

// c := &Conn{
// c: &net.TCPConn{},
// }
// c.out = make(chan *FrameHeader, 1)
// c.bw = bufio.NewWriter(bf)

// go c.writeLoop()

// framesToTest := 32

// id := uint32(1)
// frames := make([]*FrameHeader, 0, framesToTest)

// for i := 0; i < framesToTest; i++ {
// fr := AcquireFrameHeader()
// fr.SetStream(id)
// fr.SetBody(&Data{})
// id += 2
// frames = append(frames, fr)
// }

// for len(frames) > 0 {
// i := rand.Intn(len(frames))

// c.out <- frames[i]
// frames = append(frames[:i], frames[i+1:]...)
// }

// br := bufio.NewReader(bf)
// fr := AcquireFrameHeader()

// expected := uint32(1)
// for i := 0; i < framesToTest; i++ {
// _, err := fr.ReadFrom(br)
// if err != nil {
// if err == io.EOF {
// break
// }
// t.Fatalf("error reading frame: %s", err)
// }

// if fr.Stream() != expected {
// t.Fatalf("Unexpected id: %d <> %d", fr.Stream(), expected)
// }

// expected += 2
// }

// close(c.out)
// }
