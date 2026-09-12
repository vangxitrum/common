package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// minRSABits is the smallest RSA modulus this package loads or generates.
//
// RFC 7518 section 3.3 requires 2048 bits for RS256, and a 1024-bit modulus is
// within reach of a well-funded attacker. A key ring is a long-lived thing, so
// the floor is applied at load time rather than left to whoever made the file.
const minRSABits = 2048

// Suffixes of the two kinds of file a key ring directory holds.
const (
	privateSuffix = ".pem"
	publicSuffix  = ".pub.pem"
)

// KeyInfo describes one key file found by [LoadKeyRing] or written by
// [WritePrivateKey] and [WritePublicKey].
type KeyInfo struct {
	// Name is the file name without ".pem" or ".pub.pem". The active key is
	// chosen by name.
	Name string

	// ID is the "kid": the RFC 7638 JWK thumbprint of the public key. It is
	// derived from the key rather than from the file name, so renaming or
	// copying a file cannot make two keys share a kid, and every replica that
	// holds the same key computes the same one.
	ID string

	// Path is the file the key was read from or written to.
	Path string

	// Alg is the algorithm the key signs or verifies with, from [AlgorithmFor].
	Alg Algorithm

	// Private is true for "<name>.pem" and false for "<name>.pub.pem".
	Private bool

	// Active is true for the key the ring's Signer uses.
	Active bool

	// LoosePermissions is true when a private key file is readable by group
	// or other. It is a warning rather than an error, because a secret mount
	// may legitimately be 0644 inside a single-tenant container; the caller
	// decides whether to log it or refuse to start.
	LoosePermissions bool
}

// KeyRingOptions configures the Signer and the verifiers a [KeyRing] builds.
//
// The kid is always set by the ring, after these options, so an option here
// cannot make the Signer stamp a kid the ring does not route.
type KeyRingOptions struct {
	Signer   []SignerOption
	Verifier []VerifierOption
}

// KeyRing is a set of asymmetric keys loaded from a directory of PEM files:
// one active private key to sign with, and every key in the directory to
// verify with.
//
// It exists so that rotating a key is a file operation on each replica rather
// than a code change. Drop the new key in, restart; switch the active name,
// restart; delete the old key, restart. See the package README for the three
// steps and why their order matters.
//
// A KeyRing is immutable once loaded, and safe for concurrent use.
type KeyRing struct {
	signer    *Signer
	ring      *Ring
	keys      []KeyInfo
	active    KeyInfo
	activeDER []byte
	skipped   []string
}

// member is one distinct key in a ring, possibly found under several names.
type member struct {
	pub  crypto.PublicKey
	priv crypto.Signer
	alg  Algorithm
}

// LoadKeyRing reads every key in dir and signs with the private key named
// active.
//
// dir is flat. "<name>.pem" is a private key, from which the public half is
// derived; "<name>.pub.pem" is a public key only, which is how a retired key
// keeps verifying its tokens after its private half is deleted. Entries whose
// name starts with "." are ignored, which covers the "..data" symlinks a
// Kubernetes secret mount creates, and so are subdirectories. Any other file
// is listed in [KeyRing.Skipped] for the caller to log.
//
// The same key found more than once - "a.pem" and "a.pub.pem", or a copy under
// another name - is one ring member, because the kid is derived from the key.
//
// Every error names the file it is about, since the person reading it is
// looking at a directory listing.
func LoadKeyRing(dir, active string, opts KeyRingOptions) (*KeyRing, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, Error.Errorf("key directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, Error.Errorf("key directory %s is not a directory", dir)
	}
	if active == "" {
		return nil, Error.Errorf(
			"no active key named for %s; name the <name>.pem file to sign with",
			dir,
		)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, Error.Errorf("key directory %s: %w", dir, err)
	}

	k := &KeyRing{}
	members := map[string]*member{}
	var order []string

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)

		// Stat rather than entry.Type, so a symlink is judged by what it
		// points at: a Kubernetes mount is a directory of symlinks to files.
		fi, err := os.Stat(path)
		if err != nil {
			return nil, Error.Errorf("%s: %w", path, err)
		}
		if fi.IsDir() {
			continue
		}

		var base string
		var private bool
		switch {
		case strings.HasSuffix(name, publicSuffix):
			base = strings.TrimSuffix(name, publicSuffix)
		case strings.HasSuffix(name, privateSuffix):
			base, private = strings.TrimSuffix(name, privateSuffix), true
		default:
			k.skipped = append(k.skipped, name)
			continue
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, Error.Errorf("%s: %w", path, err)
		}

		var m member
		if private {
			m.priv, err = ParsePrivateKeyPEM(raw)
			if err == nil {
				m.pub = m.priv.Public()
			}
		} else {
			m.pub, err = ParsePublicKeyPEM(raw)
		}
		if err != nil {
			return nil, Error.Errorf("%s: %w", path, err)
		}

		m.alg, err = AlgorithmFor(m.pub)
		if err != nil {
			return nil, Error.Errorf("%s: %w", path, err)
		}
		kid, err := Thumbprint(m.pub)
		if err != nil {
			return nil, Error.Errorf("%s: %w", path, err)
		}

		k.keys = append(k.keys, KeyInfo{
			Name:             base,
			ID:               kid,
			Path:             path,
			Alg:              m.alg,
			Private:          private,
			LoosePermissions: private && fi.Mode().Perm()&0o077 != 0,
		})

		existing, ok := members[kid]
		switch {
		case !ok:
			members[kid] = &m
			order = append(order, kid)
		case existing.priv == nil && m.priv != nil:
			existing.priv = m.priv
		}
	}

	if len(k.keys) == 0 {
		return nil, Error.Errorf(
			"no keys in %s; expected <name>.pem or <name>.pub.pem files", dir,
		)
	}

	slices.SortFunc(k.keys, func(a, b KeyInfo) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})

	if err := checkNamesAgree(k.keys); err != nil {
		return nil, err
	}

	activePath := filepath.Join(dir, active+privateSuffix)
	activeIdx := slices.IndexFunc(k.keys, func(ki KeyInfo) bool {
		return ki.Private && ki.Name == active
	})
	if activeIdx < 0 {
		if slices.ContainsFunc(
			k.keys,
			func(ki KeyInfo) bool { return ki.Name == active },
		) {
			return nil, Error.Errorf(
				"active key %q has only a public half; signing needs %s",
				active, activePath,
			)
		}
		return nil, Error.Errorf(
			"active key %q not found: no %s",
			active,
			activePath,
		)
	}
	k.keys[activeIdx].Active = true
	k.active = k.keys[activeIdx]

	signing := members[k.active.ID]
	k.activeDER, err = x509.MarshalPKCS8PrivateKey(signing.priv)
	if err != nil {
		return nil, Error.Errorf("%s: %w", activePath, err)
	}

	signerOpts := append(slices.Clone(opts.Signer), WithKeyID(k.active.ID))
	k.signer, err = NewKeySigner(signing.alg, signing.priv, signerOpts...)
	if err != nil {
		return nil, Error.Errorf("%s: %w", activePath, err)
	}

	verifiers := make([]*Verifier, 0, len(order))
	for _, kid := range order {
		m := members[kid]
		verifierOpts := append(slices.Clone(opts.Verifier), ExpectKeyID(kid))
		v, err := NewKeyVerifier(m.alg, m.pub, verifierOpts...)
		if err != nil {
			return nil, Error.Errorf("key %s in %s: %w", kid, dir, err)
		}
		verifiers = append(verifiers, v)
	}
	k.ring, err = NewRing(verifiers...)
	if err != nil {
		return nil, err
	}

	return k, nil
}

// checkNamesAgree refuses "<name>.pem" and "<name>.pub.pem" that hold
// different keys.
//
// The usual cause is a key regenerated in place with a stale public file left
// beside it. Loading both would quietly keep trusting a key the operator
// believes was replaced, so the pair is an error rather than two members.
// keys must be sorted by name.
func checkNamesAgree(keys []KeyInfo) error {
	for i := 1; i < len(keys); i++ {
		a, b := keys[i-1], keys[i]
		if a.Name == b.Name && a.ID != b.ID {
			return Error.Errorf(
				"%s and %s share a name but hold different keys; remove the stale one",
				a.Path,
				b.Path,
			)
		}
	}
	return nil
}

// Signer returns the Signer for the active private key. It stamps the key's
// kid, plus whatever [KeyRingOptions].Signer set.
func (k *KeyRing) Signer() *Signer { return k.signer }

// Ring returns a Ring with one verifier per distinct key in the directory,
// each pinned to its own key's algorithm and built with
// [KeyRingOptions].Verifier.
func (k *KeyRing) Ring() *Ring { return k.ring }

// Keys returns every key file loaded, sorted by name. A key found under two
// names appears twice, with the same ID.
func (k *KeyRing) Keys() []KeyInfo { return slices.Clone(k.keys) }

// Active returns the key the Signer uses.
func (k *KeyRing) Active() KeyInfo { return k.active }

// Skipped returns the names of files that were ignored because they are
// neither ".pem" nor ".pub.pem", so the caller can log them. A key saved as
// "new.key" by mistake is otherwise silently absent from the ring.
func (k *KeyRing) Skipped() []string { return slices.Clone(k.skipped) }

// DeriveSecret returns 32 bytes derived from the active private key with
// HKDF-SHA256 (RFC 5869), using info to separate one use from another.
//
// It lets a service that already mounts a signing key get the symmetric
// secrets it needs - a cookie key, a CSRF key - without a second secret to
// provision and rotate. The output is deterministic for a given key and info,
// so every replica derives the same value, and it changes when the active key
// does: whatever it protects is rotated along with the signing key. Use a
// distinct info string per purpose, so one derived secret leaking reveals
// nothing about another.
func (k *KeyRing) DeriveSecret(info string) []byte {
	out, err := hkdf.Key(sha256.New, k.activeDER, nil, info, 32)
	if err != nil {
		// hkdf.Key fails only for a length above 255 hash outputs, and 32 is
		// one. Reaching this is a bug in this function, not a runtime state.
		panic(err)
	}
	return out
}

// AlgorithmFor returns the algorithm a key ring uses for pub: RS256 for an
// RSA key of at least 2048 bits, EdDSA for an Ed25519 key.
//
// RS256 rather than PS256 because RS256 is the one every JWT library and
// every OAuth 2.0 resource server supports; RFC 9068 section 4 makes it the
// mandatory algorithm for access tokens. Any other key type is an error: a
// key ring holds keys a deployment generated for this purpose, and there is
// no reason for one to be ECDSA or short.
func AlgorithmFor(pub crypto.PublicKey) (Algorithm, error) {
	switch key := pub.(type) {
	case *rsa.PublicKey:
		if bits := key.N.BitLen(); bits < minRSABits {
			return "", Error.Errorf(
				"RSA key is %d bits; at least %d are required",
				bits,
				minRSABits,
			)
		}
		return RS256, nil
	case ed25519.PublicKey:
		return EdDSA, nil
	default:
		return "", Error.Errorf(
			"unsupported key type %T; use RSA or Ed25519",
			pub,
		)
	}
}

// Thumbprint returns the RFC 7638 JWK thumbprint of pub: the unpadded
// base64url SHA-256 of the key's canonical JWK.
//
// It is what a key ring uses as the kid. Deriving the kid from the key rather
// than choosing it means two different keys cannot share one, the same key
// has the same kid on every replica with no coordination, and a JWKS consumer
// can recompute it. RSA and Ed25519 keys are supported.
func Thumbprint(pub crypto.PublicKey) (string, error) {
	var jwk any
	switch key := pub.(type) {
	case *rsa.PublicKey:
		// The canonical form is the required members only, in lexicographic
		// order, with no whitespace. A struct whose fields are declared in
		// that order marshals to exactly that.
		jwk = struct {
			E   string `json:"e"`
			Kty string `json:"kty"`
			N   string `json:"n"`
		}{
			E:   b64(big.NewInt(int64(key.E)).Bytes()),
			Kty: "RSA",
			N:   b64(key.N.Bytes()),
		}
	case ed25519.PublicKey:
		jwk = struct {
			Crv string `json:"crv"`
			Kty string `json:"kty"`
			X   string `json:"x"`
		}{
			Crv: "Ed25519",
			Kty: "OKP",
			X:   b64(key),
		}
	case *ecdsa.PublicKey:
		return "", Error.Errorf("thumbprint of an ECDSA key is unsupported")
	default:
		return "", Error.Errorf("thumbprint of %T is unsupported", pub)
	}

	canonical, err := json.Marshal(jwk)
	if err != nil {
		return "", Error.Wrap(err)
	}
	sum := sha256.Sum256(canonical)
	return b64(sum[:]), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// ParsePrivateKeyPEM parses an RSA or Ed25519 private key from PEM, either
// PKCS#8 ("PRIVATE KEY") or, for RSA, PKCS#1 ("RSA PRIVATE KEY").
//
// PKCS#1 is accepted because it is what "openssl genrsa" wrote for years and
// what most existing RSA keys look like. An encrypted key is refused: a
// service that could decrypt it would need the passphrase next to it anyway.
func ParsePrivateKeyPEM(b []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, Error.Errorf("no PEM block found")
	}

	var parsed any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, Error.Errorf(
			"PEM block is %q; want \"PRIVATE KEY\" or \"RSA PRIVATE KEY\"",
			block.Type,
		)
	}
	if err != nil {
		return nil, Error.Wrap(err)
	}

	switch key := parsed.(type) {
	case *rsa.PrivateKey:
		return key, nil
	case ed25519.PrivateKey:
		return key, nil
	default:
		return nil, Error.Errorf(
			"unsupported private key type %T; use RSA or Ed25519",
			parsed,
		)
	}
}

// ParsePublicKeyPEM parses a public key from PEM, either PKIX ("PUBLIC KEY")
// or, for RSA, PKCS#1 ("RSA PUBLIC KEY").
func ParsePublicKeyPEM(b []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, Error.Errorf("no PEM block found")
	}

	var parsed any
	var err error
	switch block.Type {
	case "PUBLIC KEY":
		parsed, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		parsed, err = x509.ParsePKCS1PublicKey(block.Bytes)
	default:
		return nil, Error.Errorf(
			"PEM block is %q; want \"PUBLIC KEY\" or \"RSA PUBLIC KEY\"",
			block.Type,
		)
	}
	if err != nil {
		return nil, Error.Wrap(err)
	}
	return parsed, nil
}
