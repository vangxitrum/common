# clientip

The address a request really came from, for a service behind reverse proxies.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/clientip"
```

`X-Forwarded-For` is a request header like any other: any client can send one.
Each proxy in front of the service appends the address it received the request
from, so only the entries **our own proxies** wrote can be believed. Walking the
list from the right and stopping at the first address that is not one of our
proxies gives the one address the client could not choose.

Reading it any other way lets a client pick the address it is rate limited and
logged by. gin trusts every proxy by default, and echo reads the header with no
trust list at all; on 2026-09-12 every gin service and stream did one or the
other, so every IP-keyed limit could be bypassed with one header.

## Usage

```go
resolver, err := clientip.New(cfg.ClientIP)
if err != nil {
    return err
}
ip := resolver.ClientIP(r) // netip.Addr
```

```yaml
client-ip:
  trusted-proxies: [10.0.0.0/8]   # the load balancers; empty believes no header
  header: X-Forwarded-For         # or X-Real-IP
```

With no trusted proxies, no header is read and the answer is the connection's
own address. That is the safe default for a service that faces clients
directly, and the wrong one for a service behind a proxy, which then sees every
user as the proxy's address - so configure it wherever there is a proxy.

## Wiring it into gin and echo

aioz-common imports neither framework; these are the few lines a service
writes.

**gin** - set gin's own trust list from the same config, so `c.ClientIP()` in
access logs agrees:

```go
resolver, err := clientip.New(cfg.ClientIP)
if err != nil {
    return err
}
if err := router.SetTrustedProxies(cfg.ClientIP.TrustedProxies); err != nil {
    return err
}

// in a handler
ip := resolver.ClientIP(c.Request)
```

**echo** - replace the IP extractor, and `c.RealIP()` is right everywhere:

```go
resolver, err := clientip.New(cfg.ClientIP)
if err != nil {
    return err
}
e.IPExtractor = func(r *http.Request) string {
    if ip := resolver.ClientIP(r); ip.IsValid() {
        return ip.String()
    }
    return ""
}
```

## The rules

- A peer that is not a trusted proxy is the client. Its headers are never read.
- `X-Forwarded-For`: every header line in order, each read from the right. The
  first address that is not a trusted proxy is the client.
- A hop that does not parse (`unknown`, empty, garbage) stops the walk at the
  last trusted address: that proxy is the answer the client did not choose.
- Every hop trusted: the leftmost, an internal caller.
- `X-Real-IP` is used only when there is exactly one; two means a proxy
  appended rather than overwrote.
- IPv4-mapped IPv6 addresses are their IPv4 address; zones are dropped; hops
  may carry a port (`1.2.3.4:80`, `[2001:db8::1]:443`).
- An invalid result (`!IsValid()`) means the connection's own address was not
  an IP, as on a unix socket. Treat it as an error, never as one shared key.

## What `New` refuses

- `0.0.0.0/0` and `::/0`: trusting every peer makes the header entirely
  client-controlled.
- The RFC 7239 `Forwarded` header: no proxy in front of our services sends it,
  and its quoted, obfuscated `for=` grammar would be attack surface with no
  caller.
- Anything that is not an address or a CIDR prefix (a bare address is one
  address: `/32` or `/128`).

## API

```go
type Config struct {
    TrustedProxies []string
    Header         string
}

func New(config Config) (*Resolver, error)
func (r *Resolver) ClientIP(req *http.Request) netip.Addr

var Error = errs.Tag("clientip")
```
