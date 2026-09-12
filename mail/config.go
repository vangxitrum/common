package mail

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
)

var mon = monkit.Package()

// The providers [New] knows.
const (
	ProviderResend = "resend" // send through Resend
	ProviderLog    = "log"    // send nothing; log that a mail was not sent
)

// Config selects and configures the provider.
//
// The provider is named, never inferred: an empty API key with the resend
// provider is a startup error, not a quiet switch to not sending, because a
// production deploy that forgot the key must fail where someone sees it.
type Config struct {
	Provider string `help:"where mail goes: resend sends it; log only records that a mail was not sent" default:"resend" devDefault:"log" testDefault:"log"`
	From     string `help:"sender for messages that set none, as Name <addr>"`

	Resend ResendConfig
}

// ResendConfig configures the Resend provider.
type ResendConfig struct {
	APIKey string `help:"Resend API key; required when the provider is resend" secret:"optional"`

	MaxAttempts  int           `help:"attempts per send, the first included"  default:"4"`
	RetryWaitMin time.Duration `help:"shortest nominal wait between attempts" default:"500ms"`
	RetryWaitMax time.Duration `help:"longest nominal wait between attempts"  default:"10s"`

	BaseURL string `help:"Resend API base URL; for tests" default:"" hidden:"true"`

	Client httpclient.Config
}

// New returns the configured provider.
func New(log *slog.Logger, config Config) (Sender, error) {
	switch config.Provider {
	case ProviderResend:
		return NewResend(log, config.From, config.Resend)
	case ProviderLog:
		return NewLog(log), nil
	default:
		return nil, fmt.Errorf("mail: provider %q is neither %q nor %q",
			config.Provider, ProviderResend, ProviderLog)
	}
}
