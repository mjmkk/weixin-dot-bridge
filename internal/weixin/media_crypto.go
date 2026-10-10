package weixin

import (
	"bufio"
	"context"
	"crypto/aes"
	"errors"
	"io"
)

// Keep read failures distinct from malformed padding. The original reader may
// carry a signed URL in its error, so only a stable category is retained.
type mediaCryptoReadError struct {
	cause error
}

func (e *mediaCryptoReadError) Error() string { return "Weixin encrypted media read interrupted" }
func (e *mediaCryptoReadError) Unwrap() error { return e.cause }

func mediaCryptoReadFailure(err error) error {
	var cause error
	if errors.Is(err, context.Canceled) {
		cause = context.Canceled
	} else if errors.Is(err, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	return &mediaCryptoReadError{cause: cause}
}

type mediaContextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r *mediaContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(p)
}

type mediaCountReader struct {
	source io.Reader
	count  int64
}

func (r *mediaCountReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	r.count += int64(n)
	return n, err
}

// Tencent's wire format uses independent AES-128 blocks with PKCS#7 padding.
// Reader/writer buffers are fixed-size; there is never an entire-file byte slice.
func encryptECB(ctx context.Context, source io.Reader, destination io.Writer, key []byte) (int64, int64, error) {
	if len(key) != 16 {
		return 0, 0, ErrMediaIntegrity
	}
	cipher, err := aes.NewCipher(key)
	if err != nil {
		return 0, 0, ErrMediaIntegrity
	}
	reader := bufio.NewReaderSize(&mediaContextReader{ctx: ctx, source: source}, 32768)
	writer := bufio.NewWriterSize(destination, 32768)
	var block [16]byte
	var plainSize, cipherSize int64
	for {
		n, err := io.ReadFull(reader, block[:])
		plainSize += int64(n)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return plainSize, cipherSize, err
		}
		final := err != nil
		if final {
			padding := byte(16 - n)
			for i := n; i < 16; i++ {
				block[i] = padding
			}
		}
		cipher.Encrypt(block[:], block[:])
		if _, err := writer.Write(block[:]); err != nil {
			return plainSize, cipherSize, err
		}
		cipherSize += 16
		if final {
			return plainSize, cipherSize, writer.Flush()
		}
	}
}

func decryptECB(ctx context.Context, source io.Reader, destination io.Writer, key []byte) (int64, error) {
	if len(key) != 16 {
		return 0, ErrMediaIntegrity
	}
	cipher, err := aes.NewCipher(key)
	if err != nil {
		return 0, ErrMediaIntegrity
	}
	reader := bufio.NewReaderSize(&mediaContextReader{ctx: ctx, source: source}, 32768)
	writer := bufio.NewWriterSize(destination, 32768)
	var current, next [16]byte
	if _, err := io.ReadFull(reader, current[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return 0, ErrMediaIntegrity
		}
		return 0, mediaCryptoReadFailure(err)
	}
	var size int64
	for {
		n, err := io.ReadFull(reader, next[:])
		if err != nil && !(err == io.EOF && n == 0) {
			if err == io.ErrUnexpectedEOF {
				return size, ErrMediaIntegrity
			}
			return size, mediaCryptoReadFailure(err)
		}
		cipher.Decrypt(current[:], current[:])
		length := 16
		if err == io.EOF {
			padding := int(current[15])
			if padding < 1 || padding > 16 {
				return size, ErrMediaIntegrity
			}
			for _, value := range current[16-padding:] {
				if int(value) != padding {
					return size, ErrMediaIntegrity
				}
			}
			length -= padding
		}
		written, writeErr := writer.Write(current[:length])
		size += int64(written)
		if writeErr != nil {
			return size, writeErr
		}
		if err == io.EOF {
			return size, writer.Flush()
		}
		current = next
	}
}
