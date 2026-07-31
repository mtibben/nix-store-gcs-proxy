package main

import (
	"fmt"
	"io"
	"sync"
)

const streamCopyBufferSize = 32 * 1024

var streamCopyBufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, streamCopyBufferSize)
		return &buffer
	},
}

func copyStream(destination io.Writer, source io.Reader) (int64, error) {
	value := streamCopyBufferPool.Get()
	buffer, ok := value.(*[]byte)
	if !ok {
		panic("stream copy buffer pool returned an unexpected value")
	}
	defer streamCopyBufferPool.Put(buffer)

	written, err := io.CopyBuffer(destination, readerOnly{Reader: source}, *buffer)
	if err != nil {
		return written, fmt.Errorf("copy stream: %w", err)
	}

	return written, nil
}

type readerOnly struct {
	io.Reader
}
