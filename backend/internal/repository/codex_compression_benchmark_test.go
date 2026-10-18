package repository

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func BenchmarkCodexCompressionLifecycle(b *testing.B) {
	body := make([]byte, 3<<20)
	random := rand.New(rand.NewPCG(17, 19))
	for i := range body {
		body[i] = byte('a' + random.IntN(26))
		if i%4096 < 1024 {
			body[i] = 'x'
		}
	}
	for _, name := range []string{"fresh", "bounded_pool"} {
		b.Run(name, func(b *testing.B) {
			pool := make(codexEncoderPool, 2)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				var encoded bytes.Buffer
				var encoder *zstd.Encoder
				var err error
				if name == "fresh" {
					encoder, err = zstd.NewWriter(&encoded,
						zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
						zstd.WithEncoderConcurrency(1))
				} else {
					encoder, err = pool.acquire(&encoded)
				}
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(encoder, codexContextReader{context.Background(), bytes.NewReader(body)}); err != nil {
					b.Fatal(err)
				}
				if err := encoder.Close(); err != nil {
					b.Fatal(err)
				}
				if name != "fresh" {
					pool.release(encoder)
				}
			}
		})
	}
}
