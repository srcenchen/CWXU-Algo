package backup

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

var archiveMagic = []byte("CWXUBAK1")

const encryptionChunkSize = 1024 * 1024

func encryptArchive(plain, key []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := encryptArchiveStream(context.Background(), bytes.NewReader(plain), &out, key); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func encryptArchiveStream(ctx context.Context, plain io.Reader, destination io.Writer, key []byte) error {
	if len(key) != 32 {
		return errors.New("AES-256 key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	noncePrefix := make([]byte, gcm.NonceSize()-4)
	if _, err := io.ReadFull(rand.Reader, noncePrefix); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	mac := hmac.New(sha256.New, hmacKey(key))
	authenticated := io.MultiWriter(destination, mac)
	if _, err := authenticated.Write(archiveMagic); err != nil {
		return err
	}
	if _, err := authenticated.Write(noncePrefix); err != nil {
		return err
	}
	buffer := make([]byte, encryptionChunkSize)
	for counter := uint32(0); ; counter++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(plain, buffer)
		if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
			return readErr
		}
		if n == 0 {
			break
		}
		nonce := append(append([]byte(nil), noncePrefix...), 0, 0, 0, 0)
		binary.BigEndian.PutUint32(nonce[len(nonce)-4:], counter)
		ciphertext := gcm.Seal(nil, nonce, buffer[:n], archiveMagic)
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(n))
		if _, err := authenticated.Write(length[:]); err != nil {
			return err
		}
		if _, err := authenticated.Write(ciphertext); err != nil {
			return err
		}
		if readErr != nil {
			break
		}
	}
	_, err = destination.Write(mac.Sum(nil))
	return err
}

// DecryptArchiveStream authenticates the complete seekable archive before
// streaming plaintext. It therefore emits no unauthenticated plaintext.
func DecryptArchiveStream(ctx context.Context, archive io.ReadSeeker, destination io.Writer, key []byte) error {
	if len(key) != 32 {
		return errors.New("AES-256 key must be 32 bytes")
	}
	size, err := archive.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if size < int64(len(archiveMagic)+8+sha256.Size) {
		return errors.New("invalid backup archive")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, hmacKey(key))
	if _, err := copyContext(ctx, mac, io.LimitReader(archive, size-sha256.Size)); err != nil {
		return err
	}
	tag := make([]byte, sha256.Size)
	if _, err := io.ReadFull(archive, tag); err != nil {
		return err
	}
	if !hmac.Equal(tag, mac.Sum(nil)) {
		return errors.New("backup archive HMAC verification failed")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	header := make([]byte, len(archiveMagic)+8)
	if _, err := io.ReadFull(archive, header); err != nil || !hmac.Equal(header[:len(archiveMagic)], archiveMagic) {
		return errors.New("invalid backup archive")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	noncePrefix := header[len(archiveMagic):]
	recordBytes := size - int64(len(header)) - sha256.Size
	for counter := uint32(0); recordBytes > 0; counter++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var length [4]byte
		if _, err := io.ReadFull(archive, length[:]); err != nil {
			return errors.New("truncated backup archive record")
		}
		recordBytes -= 4
		plainLength := int(binary.BigEndian.Uint32(length[:]))
		cipherLength := plainLength + gcm.Overhead()
		if plainLength < 1 || plainLength > encryptionChunkSize || recordBytes < int64(cipherLength) {
			return errors.New("invalid backup archive record")
		}
		ciphertext := make([]byte, cipherLength)
		if _, err := io.ReadFull(archive, ciphertext); err != nil {
			return err
		}
		recordBytes -= int64(cipherLength)
		nonce := append(append([]byte(nil), noncePrefix...), 0, 0, 0, 0)
		binary.BigEndian.PutUint32(nonce[len(nonce)-4:], counter)
		chunk, err := gcm.Open(ciphertext[:0], nonce, ciphertext, archiveMagic)
		if err != nil {
			return errors.New("backup archive authentication failed")
		}
		if _, err := destination.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

// DecryptArchive is a convenience wrapper for small in-memory callers.
func DecryptArchive(data, key []byte) ([]byte, error) {
	var plain bytes.Buffer
	if err := DecryptArchiveStream(context.Background(), bytes.NewReader(data), &plain, key); err != nil {
		return nil, err
	}
	return plain.Bytes(), nil
}

func hmacKey(key []byte) []byte {
	sum := sha256.Sum256(append(append([]byte(nil), key...), []byte("CWXU backup HMAC v1")...))
	return sum[:]
}
