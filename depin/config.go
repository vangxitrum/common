package depin

import (
	"fmt"
	"log/slog"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
)

// The providers [New] knows.
const (
	ProviderDepin = "depin" // store through the depin network
	ProviderLog   = "log"   // store nothing; log that a file was not stored
)

// Config selects and configures the provider.
//
// The provider is named, never inferred: an empty identity directory with the
// depin provider is a startup error, not a quiet switch to storing nothing,
// because a production deploy that forgot it must fail where someone sees it.
type Config struct {
	Provider string `help:"where files go: depin stores them; log only records that a file was not stored" default:"depin" devDefault:"log" testDefault:"log"`

	Client ClientConfig
}

// ClientConfig configures the depin provider.
//
// Every tuning field's zero value takes the SDK's own default, so a deployment
// sets the two required paths and nothing else until it has a reason to.
type ClientConfig struct {
	IdentityDir  string `help:"directory holding identity.cert and identity.key; required when the provider is depin" path:"true"`
	CoordPeerURL string `help:"coordinator to dial, as <coord-id>@<host:port>; required when the provider is depin"`
	PieceKeyPath string `help:"path to piece_key.json; empty looks for it in the identity directory"                  path:"true" default:""`

	DownloadBuffer            memory.Size   `help:"read-ahead held between the download pipeline and the caller's writer; 0 takes the SDK default, negative disables it"                    default:"0"`
	DownloadConcurrency       int           `help:"piece downloads in flight per segment; 0 leaves the adaptive window uncapped"                                                            default:"0"`
	GlobalDownloadConcurrency int           `help:"piece streams in flight across every download on this client; 0 takes the SDK default"                                                   default:"0"`
	DownloadTimeout           time.Duration `help:"how long a download may make no progress before it is abandoned; 0 takes the SDK default"                                                default:"0"`
	DetailedMetrics           bool          `help:"per-piece phase tracing; costs several timings and a monkit observation per piece stream, so turn it on to investigate, not to leave on" default:"false"`

	P2PListenAddr       string `help:"host:port the libp2p host listens on; empty listens on 0.0.0.0 with an ephemeral port"                              default:""`
	P2PAnnounceAddr     string `help:"public multiaddr to advertise, e.g. /ip4/203.0.113.7/tcp/7788; only set it when the host really is reachable there" default:""`
	DisableHolePunching bool   `help:"keep worker connections on the relay circuit instead of upgrading them to direct connections"                       default:"false"`

	// The coordinator RPCs are short and idempotent, so they are retried.
	// Uploads and downloads are not: the SDK retries downloads itself, and an
	// upload is too expensive to repeat blindly.
	MaxAttempts  int           `help:"attempts per coordinator call, the first included" default:"4"`
	RetryWaitMin time.Duration `help:"shortest nominal wait between attempts"            default:"200ms"`
	RetryWaitMax time.Duration `help:"longest nominal wait between attempts"             default:"5s"`
}

// Validate reports a configuration that cannot work, at startup rather than at
// the first upload.
func (c Config) Validate() error {
	switch c.Provider {
	case ProviderLog:
		return nil
	case ProviderDepin:
	default:
		return fmt.Errorf("depin: provider %q is neither %q nor %q",
			c.Provider, ProviderDepin, ProviderLog)
	}

	if c.Client.IdentityDir == "" {
		return fmt.Errorf(
			"depin: the depin provider needs an identity directory",
		)
	}
	if c.Client.CoordPeerURL == "" {
		return fmt.Errorf(
			"depin: the depin provider needs a coordinator peer URL",
		)
	}
	return nil
}

// New returns the configured provider.
func New(log *slog.Logger, config Config) (Storage, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	switch config.Provider {
	case ProviderDepin:
		return NewClient(log, config.Client)
	default:
		return NewLog(log), nil
	}
}
