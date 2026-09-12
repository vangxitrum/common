package jwt

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// KeyType is the kind of key [GenerateKey] makes.
type KeyType string

// The key types GenerateKey supports.
const (
	// KeyTypeEd25519 is the default: small keys, fast signatures, and no
	// parameters to choose.
	KeyTypeEd25519 KeyType = "ed25519"

	// KeyTypeRSA is for verifiers that do not support EdDSA. Most OAuth 2.0
	// resource servers and JWT libraries support RS256; not all support EdDSA.
	KeyTypeRSA KeyType = "rsa"
)

// defaultRSABits is the RSA size GenerateKey uses when none is given. 3072
// bits is the NIST SP 800-57 size for security past 2030; 2048 is the floor.
const defaultRSABits = 3072

// keyName is what a key file may be called: short, and safe to use as a path
// component on every file system a service runs on.
var keyName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// GenerateKey returns a new private key for a key ring.
//
// kind "" means Ed25519. For RSA, rsaBits 0 means 3072, and anything under
// 2048 is refused rather than generated, since [LoadKeyRing] would refuse to
// load it anyway.
func GenerateKey(kind KeyType, rsaBits int) (crypto.Signer, error) {
	switch kind {
	case "", KeyTypeEd25519:
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, Error.Wrap(err)
		}
		return priv, nil
	case KeyTypeRSA:
		if rsaBits == 0 {
			rsaBits = defaultRSABits
		}
		if rsaBits < minRSABits {
			return nil, Error.Errorf(
				"RSA key of %d bits refused; at least %d are required",
				rsaBits,
				minRSABits,
			)
		}
		priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
		if err != nil {
			return nil, Error.Wrap(err)
		}
		return priv, nil
	default:
		return nil, Error.Errorf("unknown key type %q; use %q or %q",
			kind, KeyTypeEd25519, KeyTypeRSA)
	}
}

// WritePrivateKey writes key as a PKCS#8 PEM file, <dir>/<name>.pem, readable
// by its owner only, creating dir with mode 0700 if it does not exist.
//
// It never overwrites. A key file that already exists may be the only copy of
// a key that signed tokens still in flight, and replacing it is exactly the
// flag-day rotation a key ring exists to avoid; pick a new name instead.
//
// name is the bare name, without ".pem". It becomes the name [LoadKeyRing]
// selects the active key by.
func WritePrivateKey(dir, name string, key crypto.Signer) (KeyInfo, error) {
	info, err := describeKey(dir, name, privateSuffix, key.Public())
	if err != nil {
		return KeyInfo{}, err
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return KeyInfo{}, Error.Wrap(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if err := writeNewFile(dir, info.Path, data, 0o600); err != nil {
		return KeyInfo{}, err
	}
	info.Private = true
	return info, nil
}

// WritePublicKey writes pub as a PKIX PEM file, <dir>/<name>.pub.pem, with
// mode 0644, creating dir with mode 0700 if it does not exist.
//
// This is the file to leave behind when a private key is retired: the key
// ring keeps verifying the tokens it signed until they expire. Like
// [WritePrivateKey], it never overwrites.
func WritePublicKey(dir, name string, pub crypto.PublicKey) (KeyInfo, error) {
	info, err := describeKey(dir, name, publicSuffix, pub)
	if err != nil {
		return KeyInfo{}, err
	}

	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return KeyInfo{}, Error.Wrap(err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})

	if err := writeNewFile(dir, info.Path, data, 0o644); err != nil {
		return KeyInfo{}, err
	}
	return info, nil
}

// describeKey validates name and pub and fills in everything a KeyInfo says
// about a key file before it is written, so an unusable key is refused
// without leaving a file behind.
func describeKey(
	dir, name, suffix string,
	pub crypto.PublicKey,
) (KeyInfo, error) {
	if err := checkKeyName(name); err != nil {
		return KeyInfo{}, err
	}
	alg, err := AlgorithmFor(pub)
	if err != nil {
		return KeyInfo{}, err
	}
	kid, err := Thumbprint(pub)
	if err != nil {
		return KeyInfo{}, err
	}
	return KeyInfo{
		Name: name,
		ID:   kid,
		Path: filepath.Join(dir, name+suffix),
		Alg:  alg,
	}, nil
}

// checkKeyName refuses a name that would not load back as the same key.
//
// A leading "." is ignored by LoadKeyRing, and a name ending in ".pem" is a
// caller passing the file name rather than the key name, which would produce
// "a.pem.pem".
func checkKeyName(name string) error {
	switch {
	case !keyName.MatchString(name):
		return Error.Errorf(
			"key name %q must be 1-64 characters of letters, digits, '.', '_' or '-'",
			name,
		)
	case strings.HasPrefix(name, "."):
		return Error.Errorf(
			"key name %q must not start with '.'; LoadKeyRing ignores such files",
			name,
		)
	case strings.HasSuffix(strings.ToLower(name), privateSuffix):
		return Error.Errorf(
			"key name %q must not end in %q; pass the bare name",
			name,
			privateSuffix,
		)
	}
	return nil
}

// writeNewFile writes data to a file that must not already exist, and removes
// it again if the write does not complete, so a failure never leaves a
// truncated key that LoadKeyRing would later refuse to parse.
func writeNewFile(dir, path string, data []byte, perm os.FileMode) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Error.Errorf("%s: %w", dir, err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) {
		return Error.Errorf(
			"%s already exists; refusing to overwrite a key",
			path,
		)
	}
	if err != nil {
		return Error.Errorf("%s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()

	// The umask applies at create time; set the mode explicitly so the file is
	// what the caller asked for whatever the process umask is.
	if err := f.Chmod(perm); err != nil {
		return Error.Errorf("%s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return Error.Errorf("%s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return Error.Errorf("%s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return Error.Errorf("%s: %w", path, err)
	}
	return nil
}
