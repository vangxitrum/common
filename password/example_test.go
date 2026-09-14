package password_test

import (
	"context"
	"errors"
	"fmt"

	"gitlab.internal/tuan.quang.tran/aioz-common/password"
)

func ExampleHasher_Verify() {
	hasher, err := password.New(password.Config{})
	if err != nil {
		panic(err)
	}
	ctx := context.Background()

	// Signup: the policy first, then the hash to store.
	if err := hasher.Check("hunter2"); errors.Is(err, password.ErrTooShort) {
		fmt.Println("too short")
	}
	stored, err := hasher.Hash(ctx, "correct horse battery staple")
	if err != nil {
		panic(err)
	}

	// Login.
	result, err := hasher.Verify(ctx, "correct horse battery staple", stored)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.OK, result.NeedsRehash)

	// No such account: "" is checked against a dummy hash, taking as long.
	result, err = hasher.Verify(ctx, "correct horse battery staple", "")
	if err != nil {
		panic(err)
	}
	fmt.Println(result.OK)
	// Output:
	// too short
	// true false
	// false
}
