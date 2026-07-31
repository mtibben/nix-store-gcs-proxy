package main

import (
	"bytes"
	"io"
	"testing"
)

const benchmarkObjectSize = 4 * 1024 * 1024

func BenchmarkObjectCopy(b *testing.B) {
	data := make([]byte, benchmarkObjectSize)
	destination := writerOnly{Writer: io.Discard}

	b.Run("io.Copy", func(b *testing.B) {
		b.SetBytes(benchmarkObjectSize)
		b.ReportAllocs()

		for b.Loop() {
			source := readerOnly{Reader: bytes.NewReader(data)}
			if _, err := io.Copy(destination, source); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("pooled buffer", func(b *testing.B) {
		b.SetBytes(benchmarkObjectSize)
		b.ReportAllocs()

		for b.Loop() {
			source := bytes.NewReader(data)
			if _, err := copyStream(destination, source); err != nil {
				b.Fatal(err)
			}
		}
	})
}

type writerOnly struct {
	io.Writer
}
