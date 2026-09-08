package secret_test

import (
	"fmt"

	"gitlab.internal/tuan.quang.tran/aioz-common/secret"
)

// The whole shape of the package: mint, hand the plaintext over once, store
// the hash, verify later.
func Example() {
	token, hash := secret.NewToken()

	// Hand token.Reveal() to whoever will present it, and store hash.
	// Nothing else keeps the plaintext.

	fmt.Println(secret.Verify(token.Reveal(), hash))
	fmt.Println(secret.Verify("a guess", hash))
	// Output:
	// true
	// false
}

// A Token does not print, log or marshal itself.
func ExampleToken() {
	token, _ := secret.NewToken()

	fmt.Printf("%s\n", token)
	fmt.Printf("%v\n", token)
	fmt.Printf("%d\n", token) // no Formatter and this would print the secret
	fmt.Println(token)

	if _, err := token.MarshalJSON(); err != nil {
		fmt.Println("marshaling refused")
	}
	if _, err := token.Value(); err != nil {
		fmt.Println("SQL parameter refused")
	}
	// Output:
	// secret.Token(redacted)
	// secret.Token(redacted)
	// secret.Token(redacted)
	// secret.Token(redacted)
	// marshaling refused
	// SQL parameter refused
}

// An API key carries its own lookup id, so verification is one indexed row
// rather than a scan over every stored hash.
func ExampleNewKey() {
	key, err := secret.NewKey("aioz")
	if err != nil {
		panic(err)
	}

	// Show key.Secret.Reveal() to its owner once. Store key.ID and key.Hash.
	ref, err := secret.ParseKey(key.Secret.Reveal())
	if err != nil {
		panic(err)
	}

	fmt.Println(ref.Prefix)
	fmt.Println(ref.ID == key.ID)
	fmt.Println(secret.Verify(key.Secret.Reveal(), key.Hash))
	// Output:
	// aioz
	// true
	// true
}

// ParseKey checks the shape and the checksum with no database, so a mistyped
// key is rejected before it costs a round trip.
func ExampleParseKey() {
	key, _ := secret.NewKey("aioz")
	presented := key.Secret.Reveal()

	mangled := presented[:len(presented)-1] + "Z"
	if _, err := secret.ParseKey(mangled); err != nil {
		fmt.Println("rejected offline")
	}

	// It is not authentication, though. A key that parses is still unproven.
	if _, err := secret.ParseKey(presented); err == nil {
		fmt.Println("well formed, and still unauthenticated")
	}
	// Output:
	// rejected offline
	// well formed, and still unauthenticated
}

// Recovery codes are read off a screen and typed back by hand, so verification
// folds the characters people confuse.
func ExampleNewCodes() {
	codes, hashes := secret.NewCodes(1)
	printed := codes[0].Reveal()

	// The same code, typed the way a tired person types it.
	typed := secret.NormalizeCode(printed)
	fmt.Println(secret.VerifyCode(typed, hashes[0]))

	// O for zero, l for one, no dashes, lower case: all still accepted.
	fmt.Println(secret.VerifyCode("o0-Il-1L-oO", secret.HashOf("00111100")))
	// Output:
	// true
	// true
}
