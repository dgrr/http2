package http2

import (
	"sort"
)

type Streams struct {
	list []*Stream
}

func (strms *Streams) Insert(s *Stream) {
	i := sort.Search(len(strms.list), func(i int) bool {
		return strms.list[i].id >= s.id
	})

	if i == len(strms.list) {
		strms.list = append(strms.list, s)
	} else {
		// TODO: overflows?
		strms.list = append(strms.list[:i+1], strms.list[i:]...)
		strms.list[i] = s
	}
}

func (strms *Streams) Del(id uint32) *Stream {
	i := sort.Search(len(strms.list), func(i int) bool {
		return strms.list[i].id >= id
	})

	if i < len(strms.list) && strms.list[i].id == id {
		strm := strms.list[i]
		strms.list = append(strms.list[:i], strms.list[i+1:]...)
		return strm
	}

	return nil
}

func (strms *Streams) Get(id uint32) *Stream {
	i := sort.Search(len(strms.list), func(i int) bool {
		return strms.list[i].id >= id
	})
	if i < len(strms.list) && strms.list[i].id == id {
		return strms.list[i]
	}

	return nil
}
