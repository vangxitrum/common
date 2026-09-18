package slack

import (
	"context"
	"log/slog"

	"gitlab.internal/tuan.quang.tran/aioz-common/random"
)

// NewLog returns the provider for development and CI: it validates a message,
// posts nothing, and logs that a message was not posted.
//
// The log line carries the channel and the sizes, and nothing else. An alert
// usually quotes whatever went wrong, and a development Slack message is the
// same message production would send, so its text stays out of the log for the
// reason given in the package doc.
func NewLog(log *slog.Logger, defaultChannel string) Notifier {
	return &logNotifier{log: log, defaultChannel: defaultChannel}
}

type logNotifier struct {
	log            *slog.Logger
	defaultChannel string
}

func (n *logNotifier) Post(
	ctx context.Context,
	msg Message,
) (Receipt, error) {
	if msg.Channel == "" {
		msg.Channel = n.defaultChannel
	}
	if err := msg.validate(); err != nil {
		return Receipt{}, err
	}

	n.log.InfoContext(ctx, "message not posted (log provider)",
		"channel", msg.Channel,
		"text_bytes", len(msg.Text),
		"block_bytes", len(msg.Blocks),
		"threaded", msg.ThreadTS != "")

	return Receipt{
		Channel: msg.Channel,
		TS:      "log-" + random.String(16, random.Base62),
	}, nil
}
