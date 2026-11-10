package nip04

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/getAlby/go-nostr"
	"github.com/stretchr/testify/require"
)

func TestSharedKeysAreTheSame(t *testing.T) {
	for i := 0; i < 100; i++ {
		sk1 := nostr.GeneratePrivateKey()
		sk2 := nostr.GeneratePrivateKey()

		pk1, err := nostr.GetPublicKey(sk1)
		require.NoError(t, err)
		pk2, err := nostr.GetPublicKey(sk2)
		require.NoError(t, err)

		ss1, err := ComputeSharedSecret(pk2, sk1)
		require.NoError(t, err)
		ss2, err := ComputeSharedSecret(pk1, sk2)
		require.NoError(t, err)

		require.Equal(t, ss1, ss2)
	}
}

func TestEncryptionAndDecryption(t *testing.T) {
	sharedSecret := make([]byte, 32)
	message := "hello hello"

	ciphertext, err := Encrypt(message, sharedSecret)
	require.NoError(t, err)

	plaintext, err := Decrypt(ciphertext, sharedSecret)
	require.NoError(t, err)

	require.Equal(t, plaintext, message, "original '%s' and decrypted '%s' messages differ", message, plaintext)
}

func TestEncryptionAndDecryptionWithMultipleLengths(t *testing.T) {
	sharedSecret := make([]byte, 32)

	for i := 0; i < 150; i++ {
		message := strings.Repeat("a", i)

		ciphertext, err := Encrypt(message, sharedSecret)
		require.NoError(t, err)

		plaintext, err := Decrypt(ciphertext, sharedSecret)
		require.NoError(t, err)

		require.Equal(t, plaintext, message, "original '%s' and decrypted '%s' messages differ", message, plaintext)
	}
}

func TestNostrToolsCompatibility(t *testing.T) {
	sk1 := "92996316beebf94171065a714cbf164d1f56d7ad9b35b329d9fc97535bf25352"
	sk2 := "591c0c249adfb9346f8d37dfeed65725e2eea1d7a6e99fa503342f367138de84"
	pk2, _ := nostr.GetPublicKey(sk2)
	shared, _ := ComputeSharedSecret(pk2, sk1)
	ciphertext := "A+fRnU4aXS4kbTLfowqAww==?iv=QFYUrl5or/n/qamY79ze0A=="
	plaintext, _ := Decrypt(ciphertext, shared)
	require.Equal(t, "hello", plaintext, "invalid decryption of nostr-tools payload")
}

func TestDecryptInvalidIVOrCiphertextLength(t *testing.T) {
	sharedSecret := make([]byte, 32)
	b64 := func(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }

	for name, content := range map[string]string{
		"short iv":             b64(16) + "?iv=" + b64(8),
		"long iv":              b64(16) + "?iv=" + b64(32),
		"empty iv":             b64(16) + "?iv=",
		"ciphertext not 16x":   b64(17) + "?iv=" + b64(16),
		"ciphertext too short": b64(5) + "?iv=" + b64(16),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotPanics(t, func() {
				_, err := Decrypt(content, sharedSecret)
				require.Error(t, err)
			})
		})
	}
}
