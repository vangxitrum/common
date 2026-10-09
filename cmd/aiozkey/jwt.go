package main

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"gitlab.internal/tuan.quang.tran/aioz-common/jwt"
)

func newJWTCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jwt",
		Short: "Keys and secrets for aioz-common/jwt",
	}
	cmd.AddCommand(newKeygenCommand(), newSecretCommand())
	return cmd
}

func newKeygenCommand() *cobra.Command {
	var (
		dir       string
		publicDir string
		name      string
		keyType   string
		rsaBits   int
	)
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Write a new signing key pair for jwt.LoadKeyRing",
		Long: `Write a new signing key pair for jwt.LoadKeyRing.

The private key goes to <dir>/<name>.pem with mode 0600, the public key to
<public-dir>/<name>.pub.pem. An existing file is never overwritten: it may be
the only copy of a key whose tokens are still in flight. Pick a new name.

The key id printed is the RFC 7638 thumbprint, the kid LoadKeyRing gives the
key and puts in every token it signs.`,
		Example: "  aiozkey jwt keygen --dir /etc/coord/jwt --name 2026-10",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if publicDir == "" {
				publicDir = dir
			}
			key, err := jwt.GenerateKey(jwt.KeyType(keyType), rsaBits)
			if err != nil {
				return err
			}
			priv, err := jwt.WritePrivateKey(dir, name, key)
			if err != nil {
				return err
			}
			pub, err := jwt.WritePublicKey(publicDir, name, key.Public())
			if err != nil {
				// The private key was created by this run a moment ago and has
				// signed nothing, so removing it loses nothing and leaves the
				// pair whole: either both files or neither.
				return errors.Join(err, os.Remove(priv.Path))
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"name:    %s\nkid:     %s\nalg:     %s\nprivate: %s\npublic:  %s\n",
				priv.Name, priv.ID, priv.Alg, priv.Path, pub.Path)
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory for the private key (created with mode 0700)")
	cmd.Flags().StringVar(&publicDir, "public-dir", "", "directory for the public key (default: --dir)")
	cmd.Flags().StringVar(&name, "name", "", "key name, e.g. 2026-10; the active-key name LoadKeyRing selects")
	cmd.Flags().StringVar(&keyType, "type", string(jwt.KeyTypeEd25519),
		fmt.Sprintf("key type: %s or %s", jwt.KeyTypeEd25519, jwt.KeyTypeRSA))
	cmd.Flags().IntVar(&rsaBits, "rsa-bits", 0, "RSA key size (default 3072, minimum 2048)")
	_ = cmd.MarkFlagRequired("dir")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func newSecretCommand() *cobra.Command {
	var (
		alg      string
		encoding string
	)
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Print a new HMAC secret for jwt.NewHMACSigner",
		Long: `Print a new HMAC secret for jwt.NewHMACSigner.

The secret is as long as the algorithm requires (32 bytes for HS256, 48 for
HS384, 64 for HS512) and is printed once, encoded, on stdout. Nothing else
keeps a copy: put it straight into the secret store, and decode it with the
same encoding before passing it to the signer.`,
		Example: "  aiozkey jwt secret --alg HS256 | vault kv put secret/coord jwt_secret=-",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			encode, err := secretEncoder(encoding)
			if err != nil {
				return err
			}
			key, err := jwt.GenerateSecret(jwt.Algorithm(strings.ToUpper(alg)))
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), encode(key))
			return err
		},
	}
	cmd.Flags().StringVar(&alg, "alg", string(jwt.HS256), "HMAC algorithm: HS256, HS384 or HS512")
	cmd.Flags().StringVar(&encoding, "encoding", "base64", "output encoding: base64 (standard, padded) or hex")
	return cmd
}

func secretEncoder(name string) (func([]byte) string, error) {
	switch name {
	case "base64":
		return base64.StdEncoding.EncodeToString, nil
	case "hex":
		return hex.EncodeToString, nil
	default:
		return nil, fmt.Errorf("unknown encoding %q; use base64 or hex", name)
	}
}
