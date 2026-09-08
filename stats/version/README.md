# stats/version

**Deprecated.** This package moved to
[`aioz-common/version`](../../version/README.md).

It moved out of `stats/` when it grew version comparison and a metrics source,
neither of which is a stats concern. Everything here is a type alias or a
forwarding function, so a value produced by one package is the same value as
the other and nothing breaks while you migrate.

```go
// before
import "gitlab.internal/tuan.quang.tran/aioz-common/stats/version"

// after
import "gitlab.internal/tuan.quang.tran/aioz-common/version"
```

One thing is not an alias: `Build`. Go has no variable aliases, so this
package's `Build` is a distinct variable initialized from the same source.
Read it and you get the same stamp; assign to it and the new package will not
see the change. Nothing should be assigning to it.

If you are wiring `-ldflags`, target the **new** package. The link-time
variables here are the new package's, so
`-X ...aioz-common/stats/version.buildVersion=` names a symbol that does not
exist and the linker will silently drop it.
