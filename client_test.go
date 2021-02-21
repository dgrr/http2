package http2

import (
	"bufio"
	"bytes"
	"io"
	"math/rand"
	"testing"
)

func TestClientWriteOrder(t *testing.T) {
	bf := bytes.NewBuffer(nil)

	c := &Client{}
	c.writer = make(chan *Frame, 1)
	c.closer = make(chan struct{}, 1)
	c.bw = bufio.NewWriter(bf)

	go c.writeLoop()

	id := uint32(1)
	frames := make([]*Frame, 0, 32)

	for i := 0; i < 32; i++ {
		fr := AcquireFrame()
		fr.SetStream(id)
		id += 2
		frames = append(frames, fr)
	}

	for len(frames) > 0 {
		i := rand.Intn(len(frames))

		c.writeFrame(frames[i])
		frames = append(frames[:i], frames[i+1:]...)
	}

	br := bufio.NewReader(bf)
	fr := AcquireFrame()

	expected := uint32(1)
	for i := 0; i < 32; i++ {
		_, err := fr.ReadFrom(br)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("error reading frame: %s", err)
		}

		if fr.Stream() != expected {
			t.Fatalf("Unexpected id: %d <> %d", fr.Stream(), expected)
		}

		expected += 2
	}

	close(c.closer)
}
