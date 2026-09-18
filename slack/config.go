package slack

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
	ProviderSlack = "slack" // post to Slack
	ProviderLog   = "log"   // post nothing; log that a message was not posted
)

// Config selects and configures the provider.
//
// The provider is named, never inferred: an empty bot token with the slack
// provider is a startup error, not a quiet switch to posting nothing, because
// a production deploy that forgot the token must fail where someone sees it.
type Config struct {
	Provider string `help:"where messages go: slack posts them; log only records that a message was not posted" default:"slack" devDefault:"log" testDefault:"log"`

	// DefaultChannel is used by a message that names none, so most callers
	// never carry a channel id.
	DefaultChannel string `help:"channel for messages that set none; a channel id is preferred over a name" default:""`

	Slack SlackConfig
}

// SlackConfig configures the slack provider.
type SlackConfig struct {
	BotToken string `help:"Slack bot token (xoxb-...); required when the provider is slack" secret:"optional"`

	MaxAttempts  int           `help:"attempts per post, the first included"  default:"4"`
	RetryWaitMin time.Duration `help:"shortest nominal wait between attempts" default:"500ms"`
	RetryWaitMax time.Duration `help:"longest nominal wait between attempts"  default:"10s"`

	BaseURL string `help:"Slack API base URL; for tests" default:"" hidden:"true"`

	Client httpclient.Config
}

// Validate reports a configuration that cannot work, at startup rather than at
// the first post.
func (c Config) Validate() error {
	switch c.Provider {
	case ProviderLog:
		return nil
	case ProviderSlack:
	default:
		return fmt.Errorf("slack: provider %q is neither %q nor %q",
			c.Provider, ProviderSlack, ProviderLog)
	}

	if c.Slack.BotToken == "" {
		return fmt.Errorf("slack: the slack provider needs a bot token")
	}
	return nil
}

// New returns the configured provider.
func New(log *slog.Logger, config Config) (Notifier, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	switch config.Provider {
	case ProviderSlack:
		return NewSlack(log, config.DefaultChannel, config.Slack)
	default:
		return NewLog(log, config.DefaultChannel), nil
	}
}
