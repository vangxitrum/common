package mail

import (
	"context"
	"log/slog"
	netmail "net/mail"
	"slices"
	"strings"

	"gitlab.internal/tuan.quang.tran/aioz-common/random"
)

// NewLog returns the provider for development and CI: it validates a message,
// sends nothing, and logs that a mail was not sent.
//
// The log line carries the recipient count and their domains, and nothing
// else - no subject, body, template data or address - because the mails a
// service sends in development are the same login codes it sends in
// production, and a code in a log is still a code.
func NewLog(log *slog.Logger) Sender {
	return &logSender{log: log}
}

type logSender struct {
	log *slog.Logger
}

func (s *logSender) Send(ctx context.Context, msg Message) (Receipt, error) {
	if err := msg.validate("", false); err != nil {
		return Receipt{}, err
	}

	s.log.InfoContext(ctx, "mail not sent (log provider)",
		"recipients", msg.recipients(), "domains", domains(msg))

	return Receipt{ID: "log-" + random.String(16, random.Base62)}, nil
}

// domains lists the distinct recipient domains, sorted.
func domains(msg Message) []string {
	var out []string
	for _, list := range [][]string{msg.To, msg.Cc, msg.Bcc} {
		for _, raw := range list {
			addr, err := netmail.ParseAddress(raw)
			if err != nil {
				continue
			}
			if at := strings.LastIndexByte(addr.Address, '@'); at >= 0 {
				out = append(out, strings.ToLower(addr.Address[at+1:]))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
