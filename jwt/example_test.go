package jwt_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"
	"gitlab.internal/tuan.quang.tran/aioz-common/secret"
)

// One trust domain, one shared secret.
func Example_sharedSecret() {
	ctx := context.Background()
	key := secret.Bytes(32)

	signer, err := jwt.NewHMACSigner(jwt.HS256, key,
		jwt.WithTTL(15*time.Minute),
		jwt.WithIssuer("coord"),
		jwt.WithAudience("edgeserver"),
	)
	if err != nil {
		panic(err)
	}

	token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
	if err != nil {
		panic(err)
	}

	verifier, err := jwt.NewHMACVerifier(jwt.HS256, key,
		jwt.RequireIssuer("coord"),
		jwt.RequireAudience("edgeserver"),
	)
	if err != nil {
		panic(err)
	}

	claims, err := verifier.Verify(ctx, token.Reveal())
	if err != nil {
		panic(err)
	}

	fmt.Println(claims.Subject)
	fmt.Println(token) // never the secret
	// Output:
	// user-1
	// secret.Token(redacted)
}

// Across a trust boundary the verifier gets only the public key, so it can
// check a token but not mint one.
func Example_publicPrivateKey() {
	ctx := context.Background()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}

	signer, err := jwt.NewKeySigner(jwt.EdDSA, private, jwt.WithTTL(time.Hour))
	if err != nil {
		panic(err)
	}
	token, err := signer.Sign(ctx, jwt.Claims{Subject: "node-7"})
	if err != nil {
		panic(err)
	}

	// This half can be handed to anyone.
	verifier, err := jwt.NewKeyVerifier(jwt.EdDSA, public)
	if err != nil {
		panic(err)
	}
	claims, err := verifier.Verify(ctx, token.Reveal())
	if err != nil {
		panic(err)
	}

	fmt.Println(claims.Subject)
	// Output: node-7
}

// The algorithm is pinned at construction, so a token signed with any other
// one is refused before any signature work.
func Example_algorithmIsPinned() {
	ctx := context.Background()

	// 64 bytes, because HS512 will not accept a secret shorter than its hash.
	key := secret.Bytes(64)

	signer, err := jwt.NewHMACSigner(jwt.HS512, key, jwt.WithTTL(time.Hour))
	if err != nil {
		panic(err)
	}
	token, err := signer.Sign(ctx, jwt.Claims{Subject: "user-1"})
	if err != nil {
		panic(err)
	}

	// A verifier built for HS256 will not accept an HS512 token, even though
	// it holds the very key that signed it.
	verifier, err := jwt.NewHMACVerifier(jwt.HS256, key)
	if err != nil {
		panic(err)
	}
	if _, err := verifier.Verify(ctx, token.Reveal()); err != nil {
		fmt.Println("refused: the header's algorithm is never trusted")
	}
	// Output: refused: the header's algorithm is never trusted
}

// Rotating a key without invalidating tokens already in flight.
func ExampleRing() {
	ctx := context.Background()
	oldKey, newKey := secret.Bytes(32), secret.Bytes(32)

	oldSigner, err := jwt.NewHMACSigner(jwt.HS256, oldKey,
		jwt.WithTTL(time.Hour), jwt.WithKeyID("2026-01"))
	if err != nil {
		panic(err)
	}
	inFlight, err := oldSigner.Sign(ctx, jwt.Claims{Subject: "user-1"})
	if err != nil {
		panic(err)
	}

	// Publish both keys to every verifier, then start signing with the new
	// one. The token minted before the switch keeps working until it expires.
	oldVerifier, err := jwt.NewHMACVerifier(jwt.HS256, oldKey, jwt.ExpectKeyID("2026-01"))
	if err != nil {
		panic(err)
	}
	newVerifier, err := jwt.NewHMACVerifier(jwt.HS256, newKey, jwt.ExpectKeyID("2026-09"))
	if err != nil {
		panic(err)
	}

	ring, err := jwt.NewRing(oldVerifier, newVerifier)
	if err != nil {
		panic(err)
	}

	claims, err := ring.Verify(ctx, inFlight.Reveal())
	if err != nil {
		panic(err)
	}
	fmt.Println(claims.Subject)
	// Output: user-1
}
