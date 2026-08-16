package cwxubak

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

func keyCheck(key []byte) error {
	if len(key) != 32 {
		return errors.New("AES-256 key must be 32 bytes")
	}
	return nil
}

// EncryptStream 将明文流加密为 .cwxubak 写入 destination。
func EncryptStream(ctx context.Context, plain io.Reader, destination io.Writer, key []byte) error {
	if err := keyCheck(key); err != nil {
		return err
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
	mac := hmac.New(sha256.New, HMACKey(key))
	authenticated := io.MultiWriter(destination, mac)
	if _, err := authenticated.Write(Magic); err != nil {
		return err
	}
	if _, err := authenticated.Write(noncePrefix); err != nil {
		return err
	}
	buffer := make([]byte, ChunkSize)
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
		ciphertext := gcm.Seal(nil, nonce, buffer[:n], Magic)
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

// DecryptStream 先对完整归档做 HMAC 校验，再流式输出明文。
// 失败时不输出任何未经认证的明文。
func DecryptStream(ctx context.Context, archive io.ReadSeeker, destination io.Writer, key []byte) error {
	if err := keyCheck(key); err != nil {
		return err
	}
	size, err := archive.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if size < int64(len(Magic)+8+sha256.Size) {
		return errors.New("invalid backup archive")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, HMACKey(key))
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
	header := make([]byte, len(Magic)+8)
	if _, err := io.ReadFull(archive, header); err != nil || !hmac.Equal(header[:len(Magic)], Magic) {
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
	noncePrefix := header[len(Magic):]
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
		if plainLength < 1 || plainLength > ChunkSize || recordBytes < int64(cipherLength) {
			return errors.New("invalid backup archive record")
		}
		ciphertext := make([]byte, cipherLength)
		if _, err := io.ReadFull(archive, ciphertext); err != nil {
			return err
		}
		recordBytes -= int64(cipherLength)
		nonce := append(append([]byte(nil), noncePrefix...), 0, 0, 0, 0)
		binary.BigEndian.PutUint32(nonce[len(nonce)-4:], counter)
		chunk, err := gcm.Open(ciphertext[:0], nonce, ciphertext, Magic)
		if err != nil {
			return errors.New("backup archive authentication failed")
		}
		if _, err := destination.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}

// Decrypt 内存便捷封装。
func Decrypt(data, key []byte) ([]byte, error) {
	var plain bytes.Buffer
	if err := DecryptStream(context.Background(), bytes.NewReader(data), &plain, key); err != nil {
		return nil, err
	}
	return plain.Bytes(), nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := destination.Write(buffer[:n]); err != nil {
				return written, err
			}
			written += int64(n)
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
