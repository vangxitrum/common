package logging_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs/v2"

	"gitlab.internal/tuan.quang.tran/aioz-common/logging"
)

// End to end through the configured logger, read back from its file sink: an
// errs error is written as its message, not as {}, and without its stack.
func TestTheServiceLoggerLogsAnErrorAsItsMessage(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "service.log")
	cfg := defaults()
	cfg.File = logging.FileConfig{
		Enabled:       true,
		Path:          path,
		Format:        "json",
		BufferSize:    1024,
		FlushInterval: time.Hour,
		MaxSize:       1 << 20,
	}
	handler, _, err := logging.New(cfg)
	require.NoError(t, err)

	failure := errs.Tag("user").
		Wrap(errors.New(`relation "refresh_tokens" does not exist`))
	slog.New(handler).With("chore", "user:delete-expired-refresh-tokens").
		Error("chore pass failed; retrying next interval", "err", failure)
	require.NoError(t, shutdown(handler))

	file, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	require.True(t, scanner.Scan())

	var line map[string]any
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &line))
	require.Equal(
		t,
		`user: relation "refresh_tokens" does not exist`,
		line["err"],
	)
	require.Equal(t, "user:delete-expired-refresh-tokens", line["chore"])
	require.NotContains(t, line, "stacktrace")
}
