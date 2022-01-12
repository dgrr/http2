package http2

type Streams []*Stream

func (s *Streams) Search(id uint32) *Stream {
	for _, strm := range *s {
		if strm.ID() == id {
			return strm
		}
	}
	return nil
}

func (s *Streams) Del(id uint32) {
	if len(*s) == 1 && (*s)[0].ID() == id {
		*s = (*s)[:0]
		return
	}

	for i, strm := range *s {
		if strm.ID() == id {
			*s = append((*s)[:i], (*s)[i+1:]...)
			return
		}
	}
}
