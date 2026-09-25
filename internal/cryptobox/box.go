// Package cryptobox implements the lightweight at-rest encryption used by tinydb.
//
// File format (all integers big-endian):
//
//	header:
//	  magic      8 bytes  "WEBDBENC"
//	  version    1 byte   0x01
//	  kdf        1 byte   0 = raw 32-byte key, 1 = stretched passphrase
//	  chunkSize  4 bytes  plaintext chunk size (default 64 KiB)
//	  salt       16 bytes random (present only when kdf = 1)
//	stream of chunks, indexed from 0:
//	  flag       1 byte   1 = more chunks follow, 0 = final chunk
//	  nonce      12 bytes random
//	  len        4 bytes  ciphertext length (plaintext + 16 byte GCM tag)
//	  data       len bytes AES-256-GCM ciphertext
//
// Each chunk is sealed with AAD = magic || version || chunkIndex || flag,
// so reordering, truncation or splicing chunks is detected on decrypt.
package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	magic        = "WEBDBENC"
	version      = byte(0x01)
	kdfRaw       = byte(0x00)
	kdfStretched = byte(0x01)

	defaultChunk = 64 * 1024
	saltLen      = 16
	keyLen       = 32
	nonceLen     = 12
	tagLen       = 16
)

var (
	// ErrBadMagic means the input is not a tinydb encrypted file.
	ErrBadMagic = errors.New("cryptobox: not a tinydb encrypted file")
	// ErrCorrupt means authentication failed (wrong key or damaged data).
	ErrCorrupt = errors.New("cryptobox: data corrupt or wrong key")
)

// stretchPass is the lightweight KDF: iterated SHA-256 with salt.
// Deliberately simple: fast on phones, ~100k rounds (~50-100 ms on ARM64).
const stretchPass = 100000

// GenerateKey returns a fresh random 32-byte key.
func GenerateKey() ([]byte, error) {
	k := make([]byte, keyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// LoadOrCreateKeyFile loads a 32-byte key from path, creating it (mode 0600) if absent.
func LoadOrCreateKeyFile(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != keyLen {
			return nil, fmt.Errorf("cryptobox: key file %s must be exactly %d bytes, got %d", path, keyLen, len(b))
		}
		return b, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// DeriveKey stretches a passphrase into a 32-byte key with a random salt.
func DeriveKey(passphrase string, salt []byte) []byte {
	// KDF domain separator: keep it stable — changing this value rekeys the
	// KDF and makes every existing .enc container unreadable.
	sum := sha256.Sum256(append([]byte(passphrase+"|web-db|"), salt...))
	k := sum
	for i := 1; i < stretchPass; i++ {
		k = sha256.Sum256(append(k[:], salt...))
	}
	return k[:]
}

type header struct {
	kdf       byte
	chunkSize uint32
	salt      []byte
}

func writeHeader(w io.Writer, h header) error {
	buf := make([]byte, 0, 8+1+1+4+saltLen)
	buf = append(buf, magic...)
	buf = append(buf, version, h.kdf)
	var cs [4]byte
	binary.BigEndian.PutUint32(cs[:], h.chunkSize)
	buf = append(buf, cs[:]...)
	if h.kdf == kdfStretched {
		buf = append(buf, h.salt...)
	}
	_, err := w.Write(buf)
	return err
}

func readHeader(r io.Reader) (header, error) {
	fixed := make([]byte, 8+1+1+4)
	if _, err := io.ReadFull(r, fixed); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return header{}, ErrBadMagic
		}
		return header{}, err
	}
	if string(fixed[:8]) != magic {
		return header{}, ErrBadMagic
	}
	if fixed[8] != version {
		return header{}, fmt.Errorf("cryptobox: unsupported version %d", fixed[8])
	}
	h := header{
		kdf:       fixed[9],
		chunkSize: binary.BigEndian.Uint32(fixed[10:14]),
	}
	if h.kdf != kdfRaw && h.kdf != kdfStretched {
		return header{}, fmt.Errorf("cryptobox: unknown kdf %d", h.kdf)
	}
	if h.chunkSize == 0 || h.chunkSize > 8<<20 {
		return header{}, fmt.Errorf("cryptobox: bad chunk size %d", h.chunkSize)
	}
	if h.kdf == kdfStretched {
		h.salt = make([]byte, saltLen)
		if _, err := io.ReadFull(r, h.salt); err != nil {
			return header{}, err
		}
	}
	return h, nil
}

func aad(index uint64, flag byte) []byte {
	a := make([]byte, 0, 8+1+8+1)
	a = append(a, magic...)
	a = append(a, version)
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], index)
	a = append(a, idx[:]...)
	a = append(a, flag)
	return a
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptKey resolves how a key is stored in the file header.
// If passphrase is non-empty the file is self-describing (random salt included);
// otherwise a raw key (key file) is assumed.
func resolveKey(passphrase string) (key []byte, kdf byte, salt []byte, err error) {
	if passphrase != "" {
		salt = make([]byte, saltLen)
		if _, err := rand.Read(salt); err != nil {
			return nil, 0, nil, err
		}
		return DeriveKey(passphrase, salt), kdfStretched, salt, nil
	}
	return nil, kdfRaw, nil, nil
}

// Encrypt encrypts plaintext from src and writes the tinydb container to dst.
// Exactly one of key (32 bytes) or passphrase must be provided.
func Encrypt(src io.Reader, dst io.Writer, key []byte, passphrase string, chunkSize int) error {
	var kdf byte
	var salt []byte
	if passphrase != "" {
		var err error
		key, kdf, salt, err = resolveKey(passphrase)
		if err != nil {
			return err
		}
	} else {
		kdf = kdfRaw
	}
	if len(key) != keyLen {
		return fmt.Errorf("cryptobox: key must be %d bytes", keyLen)
	}
	if chunkSize <= 0 {
		chunkSize = defaultChunk
	}
	if err := writeHeader(dst, header{kdf: kdf, chunkSize: uint32(chunkSize), salt: salt}); err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	var index uint64
	for {
		n, rerr := io.ReadFull(src, buf)
		if rerr == io.EOF {
			// Empty plaintext: write a single final empty chunk for a well-formed stream.
			if index == 0 {
				return sealChunk(dst, gcm, index, 0, nil)
			}
			return nil
		}
		if rerr != nil && rerr != io.ErrUnexpectedEOF {
			return rerr
		}
		chunk := buf[:n]
		flag := byte(1)
		if rerr == io.ErrUnexpectedEOF {
			flag = 0 // final partial chunk
		}
		if err := sealChunk(dst, gcm, index, flag, chunk); err != nil {
			return err
		}
		index++
		if flag == 0 {
			return nil
		}
		// If the source ended exactly at a chunk boundary we must emit a final
		// empty chunk so the reader knows the stream is complete.
		if rerr == io.ErrUnexpectedEOF {
			return nil
		}
	}
}

func sealChunk(dst io.Writer, gcm cipher.AEAD, index uint64, flag byte, plain []byte) error {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, plain, aad(index, flag))
	rec := make([]byte, 1+nonceLen+4+len(ct))
	rec[0] = flag
	copy(rec[1:1+nonceLen], nonce)
	binary.BigEndian.PutUint32(rec[1+nonceLen:1+nonceLen+4], uint32(len(ct)))
	copy(rec[1+nonceLen+4:], ct)
	_, err := dst.Write(rec)
	return err
}

// Decrypt reads a tinydb container from src and writes plaintext to dst.
// key must match the key used for Encrypt unless a passphrase is supplied
// (the salt from the header is used to re-derive it).
func Decrypt(src io.Reader, dst io.Writer, key []byte, passphrase string) error {
	h, err := readHeader(src)
	if err != nil {
		return err
	}
	if h.kdf == kdfStretched {
		if passphrase == "" {
			return errors.New("cryptobox: file is passphrase-protected but no passphrase given")
		}
		key = DeriveKey(passphrase, h.salt)
	}
	if len(key) != keyLen {
		return fmt.Errorf("cryptobox: key must be %d bytes", keyLen)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	var index uint64
	for {
		fixed := make([]byte, 1+nonceLen+4)
		if _, err := io.ReadFull(src, fixed); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: truncated stream at chunk %d", ErrCorrupt, index)
			}
			return err
		}
		flag := fixed[0]
		if flag != 0 && flag != 1 {
			return fmt.Errorf("%w: bad flag at chunk %d", ErrCorrupt, index)
		}
		nonce := fixed[1 : 1+nonceLen]
		ctLen := binary.BigEndian.Uint32(fixed[1+nonceLen : 1+nonceLen+4])
		if ctLen < tagLen || int(ctLen) > int(h.chunkSize)+tagLen {
			return fmt.Errorf("%w: bad length %d at chunk %d", ErrCorrupt, ctLen, index)
		}
		ct := make([]byte, ctLen)
		if _, err := io.ReadFull(src, ct); err != nil {
			return fmt.Errorf("%w: truncated chunk %d: %v", ErrCorrupt, index, err)
		}
		plain, err := gcm.Open(nil, nonce, ct, aad(index, flag))
		if err != nil {
			return fmt.Errorf("%w at chunk %d: %v", ErrCorrupt, index, err)
		}
		if _, err := dst.Write(plain); err != nil {
			return err
		}
		if flag == 0 {
			return nil
		}
		index++
	}
}
