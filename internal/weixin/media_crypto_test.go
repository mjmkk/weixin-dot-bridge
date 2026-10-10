package weixin

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

type tinyMediaReader struct{ io.Reader }

func (r tinyMediaReader) Read(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return r.Reader.Read(p)
}

func TestMediaECBMatchesAESKnownVector(t *testing.T) {
	key, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	plaintext, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172a")
	want, _ := hex.DecodeString("3ad77bb40d7a3660a89ecaf32466ef97")
	var encrypted bytes.Buffer
	raw, size, err := encryptECB(context.Background(), bytes.NewReader(plaintext), &encrypted, key)
	if err != nil || raw != 16 || size != 32 || !bytes.Equal(encrypted.Bytes()[:16], want) {
		t.Fatalf("AES-128-ECB vector or whole-block padding failed: raw=%d size=%d err=%v", raw, size, err)
	}
}

func TestMediaECBPartialReadsAndPaddingBoundaries(t *testing.T) {
	key := []byte("0123456789abcdef")
	for _, size := range []int{0, 1, 15, 16, 17, 65537} {
		plain := bytes.Repeat([]byte{0xab}, size)
		var encrypted, decrypted bytes.Buffer
		raw, ciphertext, err := encryptECB(context.Background(), tinyMediaReader{bytes.NewReader(plain)}, &encrypted, key)
		if err != nil || raw != int64(size) || ciphertext != int64(size/16+1)*16 {
			t.Fatalf("encrypt size=%d: raw=%d cipher=%d err=%v", size, raw, ciphertext, err)
		}
		got, err := decryptECB(context.Background(), tinyMediaReader{bytes.NewReader(encrypted.Bytes())}, &decrypted, key)
		if err != nil || got != int64(size) || !bytes.Equal(plain, decrypted.Bytes()) {
			t.Fatalf("decrypt size=%d: got=%d err=%v", size, got, err)
		}
	}
}

func TestMediaECBRejectsTruncationAndInvalidPadding(t *testing.T) {
	key := []byte("0123456789abcdef")
	var encrypted bytes.Buffer
	_, _, _ = encryptECB(context.Background(), bytes.NewReader([]byte("payload")), &encrypted, key)
	badPadding := bytes.Clone(encrypted.Bytes())
	badPadding[len(badPadding)-1] ^= 0xff
	for _, data := range [][]byte{nil, encrypted.Bytes()[:15], append(bytes.Clone(encrypted.Bytes()), 1), badPadding} {
		_, err := decryptECB(context.Background(), bytes.NewReader(data), io.Discard, key)
		if !errors.Is(err, ErrMediaIntegrity) {
			t.Fatalf("invalid ciphertext accepted: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := encryptECB(ctx, bytes.NewReader([]byte("payload")), io.Discard, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("encryption ignored cancellation: %v", err)
	}
}

type repeatedMediaByte byte

func (b repeatedMediaByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func TestMediaECBStreamsLargeUnalignedSource(t *testing.T) {
	// This fixture matches the observed large attachment byte count. It tests
	// buffering/block rollover without allocating the complete source or output.
	const rawSize = int64(131868123)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	type result struct {
		raw, cipher int64
		err         error
	}
	completed := make(chan result, 1)
	go func() {
		raw, ciphertext, err := encryptECB(ctx, io.LimitReader(repeatedMediaByte(0x71), rawSize), writer, []byte("0123456789abcdef"))
		writer.CloseWithError(err)
		completed <- result{raw, ciphertext, err}
	}()
	size, err := decryptECB(ctx, reader, io.Discard, []byte("0123456789abcdef"))
	reader.CloseWithError(err)
	encoded := <-completed
	if err != nil || encoded.err != nil || encoded.raw != rawSize || size != rawSize || encoded.cipher != (rawSize/16+1)*16 {
		t.Fatalf("large block stream failed: plain=%d raw=%d cipher=%d decryptErr=%v encryptErr=%v", size, encoded.raw, encoded.cipher, err, encoded.err)
	}
}
