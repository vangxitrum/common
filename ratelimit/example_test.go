package ratelimit_test

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/ratelimit"
)

func ExampleLimiter_Allow() {
	// nil client: memory only, as in a test or a single replica.
	limiter, err := ratelimit.New(
		slog.New(slog.DiscardHandler),
		ratelimit.Config{},
	)
	if err != nil {
		panic(err)
	}

	var otpEmail ratelimit.Rule
	otpEmail.Name = "otp-email"
	if err := otpEmail.Rates.Set("1/1m,5/1h"); err != nil {
		panic(err)
	}
	key := otpEmail.Key("alice@example.com")

	for range 2 {
		result, err := limiter.Allow(context.Background(), key)
		if err != nil {
			panic(err)
		}
		if result.Allowed {
			fmt.Println("sent")
			continue
		}
		fmt.Printf("429, Retry-After: %d (within %v)\n",
			result.RetryAfterSeconds(), result.RetryAfter.Round(time.Minute))
	}
	// Output:
	// sent
	// 429, Retry-After: 60 (within 1m0s)
}
