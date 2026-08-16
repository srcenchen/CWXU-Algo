package opsinstall

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"cwxu-algo/deploy"
)

func ReadAsset(relative string) ([]byte, error) {
	data, err := deploy.Assets.ReadFile(relative)
	if err != nil {
		return nil, fmt.Errorf("嵌入资产 %s：%w", relative, err)
	}
	return data, nil
}

func randomBytes(size int) ([]byte, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return nil, fmt.Errorf("生成随机字节：%w", err)
	}
	return buffer, nil
}

func writeSecret(root string, relative string, size int) error {
	target := filepath.Join(root, relative)
	if info, err := os.Stat(target); err == nil && info.Size() > 0 {
		os.Chmod(target, 0o600)
		return nil
	}
	content, err := randomBytes(size)
	if err != nil {
		return err
	}
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

func writeHexSecret(root string, relative string, size int) error {
	target := filepath.Join(root, relative)
	if info, err := os.Stat(target); err == nil && info.Size() > 0 {
		os.Chmod(target, 0o600)
		return nil
	}
	raw, err := randomBytes(size)
	if err != nil {
		return err
	}
	hexEncoded := []byte(hex.EncodeToString(raw))
	hexEncoded = append(hexEncoded, '\n')
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, hexEncoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

func generateRSAKeyPair(root string, bits int) error {
	privatePath := filepath.Join(root, "secrets", "jwt_private_key.pem")
	if _, err := os.Stat(privatePath); err == nil {
		os.Chmod(privatePath, 0o600)
		if _, err := os.Stat(filepath.Join(root, "secrets", "jwt_public_key.pem")); err == nil {
			return nil
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		return fmt.Errorf("生成 RSA 密钥：%w", err)
	}
	privateDER := x509.MarshalPKCS1PrivateKey(key)
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privateDER})
	if err := atomicWrite(privatePath, privatePEM, 0o600); err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := atomicWrite(filepath.Join(root, "secrets", "jwt_public_key.pem"), publicPEM, 0o600); err != nil {
		return err
	}
	return nil
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}

func readSecret(root string, relative string) (string, error) {
	content, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	return string(content), nil
}
