# aiozkey

Makes the keys and secrets aioz-common's packages load, so what it writes is exactly what they read back.

It is a thin shell over the library: everything that decides what a key looks like lives in [`jwt`](../../jwt/README.md) and is tested there.
A hand-rolled `openssl` recipe drifts from what `jwt.LoadKeyRing` expects, and a service finds out only when it fails to start.

## Download

An opt-in pipeline builds it for `linux-amd64`, `linux-arm64` and `darwin-arm64` and uploads it to the team file server:

```
http://10.0.0.106:8021/aioz-common/aiozkey/<tag or branch>/<short sha>/<os>-<arch>/aiozkey
http://10.0.0.106:8021/aioz-common/aiozkey/<tag or branch>/<short sha>/SHA256SUMS
```

```bash
base=http://10.0.0.106:8021/aioz-common/aiozkey/v0.5.0/abc1234
mkdir -p linux-amd64
curl -fsSo linux-amd64/aiozkey "$base/linux-amd64/aiozkey"
curl -fsS "$base/SHA256SUMS" | grep linux-amd64 | sha256sum -c -
chmod +x linux-amd64/aiozkey
```

Check the checksum before running it: the file server is plain HTTP.

Or build it from a checkout with `go build ./cmd/aiozkey`, or every platform at once with `make tools` (into `dist/`).

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

## Publishing a build

The `tools:build` job in `.gitlab-ci.yml` runs only when a pipeline is started from **CI/CD -> Pipelines -> Run pipeline** with `BUILD_TOOLS` set to `true`, and only on the default branch or a tag.
It runs `make tools`, then `scripts/ci/upload-tool.sh` uploads `dist/aiozkey` over FTP.
It needs the project's `FTP_USER` and `FTP_PASSWORD` CI/CD variables, protected and masked.
`TOOLS_FTP_HOST` (default `10.0.0.106`), `TOOLS_FTP_PORT` (default `21`) and `TOOLS_FTP_BASE` (default `/aioznode`, the directory the `:8021` server serves) override the destination.
