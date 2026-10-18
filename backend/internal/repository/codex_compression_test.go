package repository

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type failingCodexCompressionWriter struct{}

func (failingCodexCompressionWriter) Write([]byte) (int, error) {
	return 0, errors.New("fixture write failure")
}

func TestCodexEncoderResetIsIndependentAfterFailure(t *testing.T) {
	pool := make(codexEncoderPool, 2)
	encoder, err := pool.acquire(failingCodexCompressionWriter{})
	require.NoError(t, err)
	_, _ = encoder.Write(bytes.Repeat([]byte("previous-account-private-fixture"), 100))
	require.Error(t, encoder.Close())
	pool.release(encoder)
	previous := encoder

	body := bytes.Repeat([]byte("next-account-independent-body"), 300)
	var reused bytes.Buffer
	encoder, err = pool.acquire(&reused)
	require.NoError(t, err)
	require.Same(t, previous, encoder)
	_, err = encoder.Write(body)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	pool.release(encoder)

	var fresh bytes.Buffer
	independent, err := zstd.NewWriter(&fresh, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)),
		zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	_, err = independent.Write(body)
	require.NoError(t, err)
	require.NoError(t, independent.Close())
	require.Equal(t, fresh.Bytes(), reused.Bytes(), "previous stream must not influence the next frame")
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(reused.Bytes(), nil)
	require.NoError(t, err)
	require.Equal(t, body, decoded)
}

func TestCodexEncoderConcurrentStreamsAndIdleBound(t *testing.T) {
	const workers = 8
	pool := make(codexEncoderPool, 2)
	var pending sync.WaitGroup
	errorsFound := make(chan error, workers)
	for index := range workers {
		pending.Go(func() {
			body := bytes.Repeat([]byte{byte('a' + index)}, 384<<10)
			var encoded bytes.Buffer
			encoder, err := pool.acquire(&encoded)
			if err != nil {
				errorsFound <- err
				return
			}
			_, writeErr := io.Copy(encoder, bytes.NewReader(body))
			closeErr := encoder.Close()
			pool.release(encoder)
			if err := errors.Join(writeErr, closeErr); err != nil {
				errorsFound <- err
				return
			}
			decoder, err := zstd.NewReader(nil)
			if err != nil {
				errorsFound <- err
				return
			}
			defer decoder.Close()
			decoded, err := decoder.DecodeAll(encoded.Bytes(), nil)
			if err != nil || !bytes.Equal(body, decoded) {
				errorsFound <- errors.New("concurrent independent stream mismatch")
			}
		})
	}
	pending.Wait()
	close(errorsFound)
	for err := range errorsFound {
		require.NoError(t, err)
	}
	require.Equal(t, 2, cap(pool))
	require.LessOrEqual(t, len(pool), 2)
}
