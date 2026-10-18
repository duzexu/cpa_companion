package companion

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

func keyVaultAEAD(cookieKey string) (cipher.AEAD, error) {
	if cookieKey == "" {
		return nil, errors.New("key vault secret is missing")
	}
	key := sha256.Sum256([]byte("cpa-companion-key-vault-v1:" + cookieKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealAPIKey(cookieKey, userID, keyID, plain string) (string, error) {
	aead, err := keyVaultAEAD(cookieKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plain), []byte(userID+":"+keyID))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openAPIKey(cookieKey, userID, keyID, encoded string) (string, error) {
	aead, err := keyVaultAEAD(cookieKey)
	if err != nil {
		return "", err
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(sealed) < aead.NonceSize() {
		return "", errors.New("invalid encrypted API key")
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(userID+":"+keyID))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
