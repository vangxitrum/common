package random_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strings"

	"gitlab.internal/tuan.quang.tran/aioz-common/random"
)

func ExampleBytes() {
	key := random.Bytes(32) // an HMAC key, or an HS256 secret
	fmt.Println(len(key))
	// Output: 32
}

func ExampleIntN() {
	fmt.Println(random.IntN(6) < 6)
	// Output: true
}

func ExampleString() {
	s := random.String(20, random.Base62)
	fmt.Println(len(s), strings.Trim(s, string(random.Base62)) == "")
	// Output: 20 true
}

// A one-time code is generated here and stored as a keyed HMAC, never as
// itself: six digits are only a million values, so an unkeyed hash of one is
// reversed by trying them all.
func ExampleDigits() {
	serverKey := random.Bytes(32) // kept out of the database

	code := random.Digits(6) // send this to the user
	stored := sign(serverKey, code)

	typed := code // what the user typed back
	fmt.Println(len(code), hmac.Equal(stored, sign(serverKey, typed)))
	// Output: 6 true
}

func sign(key []byte, code string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(code))
	return mac.Sum(nil)
}
