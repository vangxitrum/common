// Command aiozkey makes the keys and secrets aioz-common's packages read, so
// what it writes is exactly what they load: a jwt key pair named and laid out
// the way jwt.LoadKeyRing expects, an HMAC secret as long as
// jwt.NewHMACSigner requires.
//
// It is a thin shell over the library. Everything that decides what a key
// looks like lives in the package that later reads it, and is tested there;
// this command only parses flags and prints.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"gitlab.internal/tuan.quang.tran/aioz-common/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "aiozkey:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return newRootCommand().ExecuteContext(ctx)
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "aiozkey",
		Short: "Make the keys and secrets aioz-common services load",
		// A failed key write is not a usage mistake; printing the usage after
		// it would bury the one line that says what went wrong.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newJWTCommand(), newVersionCommand())
	return root
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build stamp",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprint(cmd.OutOrStdout(), version.Build.String())
			return err
		},
	}
}
