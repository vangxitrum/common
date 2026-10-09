# aiozkey

Makes the keys and secrets aioz-common's packages load, so what it writes is exactly what they read back.

It is a thin shell over the library: everything that decides what a key looks like lives in [`jwt`](../../jwt/README.md) and is tested there.
A hand-rolled `openssl` recipe drifts from what `jwt.LoadKeyRing` expects, and a service finds out only when it fails to start.

## Install

```bash
go install gitlab.internal/tuan.quang.tran/aioz-common/tools/aiozkey@latest
```

It needs Go 1.26 or newer and the setup every private module here needs: `GOPRIVATE=gitlab.internal` and git access to `gitlab.internal`.
`@latest` is the newest `tools/aiozkey/v*` tag; pin one, such as `@v0.1.0`, to get the same binary every time.
`aiozkey version` prints the version that was installed.

It is a module of its own, apart from aioz-common's root module, because the root `go.mod` carries a `replace` directive (gogo/protobuf, for depin's go-sdk) and `go install <path>@<version>` refuses any module that has one.
It requires a released aioz-common, so what it builds is what its tests ran against.

## jwt keygen

```bash
aiozkey jwt keygen --dir /etc/coord/jwt --name 2026-10
```

```
name:    2026-10
kid:     PpibHNGWrO8dTCtm_FC1Zu4V0MM0pbAAmIkrJ6DLEkQ
alg:     EdDSA
private: /etc/coord/jwt/2026-10.pem
public:  /etc/coord/jwt/2026-10.pub.pem
```

It writes a signing key pair in the layout `jwt.LoadKeyRing` reads: `<name>.pem` with mode 0600 and `<name>.pub.pem` with mode 0644.
The `kid` is the key's RFC 7638 thumbprint, the one `LoadKeyRing` assigns and every token it signs carries.
The name is what you pass to `LoadKeyRing` as the active key.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dir` | required | Directory for the private key, created with mode 0700. |
| `--name` | required | Key name, for example `2026-10`. |
| `--public-dir` | `--dir` | Directory for the public key; a separate one is a verify-only key ring. |
| `--type` | `ed25519` | `ed25519`, or `rsa` for verifiers without EdDSA support. |
| `--rsa-bits` | `3072` | RSA size; under 2048 is refused. |

An existing file is never overwritten, because it may be the only copy of a key whose tokens are still in flight; pick a new name.
If the public key cannot be written, the private key this run just created is removed again, so a run leaves both files or neither.
For rotating keys, see the three-step procedure in the [`jwt` README](../../jwt/README.md).

## jwt secret

```bash
aiozkey jwt secret --alg HS256
```

It prints a random HMAC secret of exactly the length `jwt.NewHMACSigner` requires (32 bytes for HS256, 48 for HS384, 64 for HS512) once, on stdout.
Nothing else keeps a copy, so pipe it straight into the secret store.
The secret is never a flag value, so it never lands in shell history.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--alg` | `HS256` | `HS256`, `HS384` or `HS512`. |
| `--encoding` | `base64` | `base64` (standard, padded) or `hex`. The service must decode with the same one. |

## version

`aiozkey version` prints the build stamp from [`version`](../../version/README.md): version, commit, build time and Go version.

## Releasing

Tag the commit with `tools/aiozkey/vX.Y.Z`, the prefix Go looks for in a nested module, and push the tag; `@latest` moves to it.
The root module's `vX.Y.Z` tags do not release aiozkey, and its tags do not release aioz-common.

CI lints and tests it with `GOWORK=off` (`make lint-modules test-tools`), against the aioz-common version its `go.mod` requires.
When it needs a change to aioz-common that is not released yet, build against the checkout with a local workspace, which `.gitignore` keeps out of commits:

```bash
go work init . ./tools/aiozkey
```

Release aioz-common first, then `go get gitlab.internal/tuan.quang.tran/aioz-common@<that tag>` here, then tag aiozkey.
