package repository

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

// Retain at most two idle encoders. Busy requests allocate independently rather
// than waiting for another account; Reset starts a new, dictionary-free stream.
var idleCodexEncoders = make(codexEncoderPool, 2)

type codexEncoderPool chan *zstd.Encoder

func (pool codexEncoderPool) acquire(writer io.Writer) (*zstd.Encoder, error) {
	select {
	case encoder := <-pool:
		encoder.Reset(writer)
		return encoder, nil
	default:
		return zstd.NewWriter(writer, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
			zstd.WithEncoderConcurrency(1))
	}
}

// Call only after Close. Detach the completed request's output before retaining
// an encoder, including cancellation and preparation failures.
func (pool codexEncoderPool) release(encoder *zstd.Encoder) {
	encoder.Reset(io.Discard)
	select {
	case pool <- encoder:
	default:
	}
}
