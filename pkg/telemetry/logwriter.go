package telemetry

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// LogSink returns an io.Writer that emits every line written to it as an
// OpenTelemetry log record. It plays nicely as a leaf of io.MultiWriter:
// callers keep their existing stderr / ring-buffer writers and just append
// this one.
//
// When telemetry.Setup has not been called (or the OTel global logger
// provider is the no-op default), Emit is a no-op, so adding LogSink to a
// MultiWriter is safe in all builds.
//
// Severity is inferred from the line prefix using cheap heuristics — the
// existing log.Printf call sites stay unchanged.
func LogSink() io.Writer {
	return &otelLogWriter{}
}

type otelLogWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	logger otellog.Logger
}

func (w *otelLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := len(p)
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(data[:idx])
		w.buf.Next(idx + 1)
		w.emit(line)
	}
	return n, nil
}

func (w *otelLogWriter) emit(line string) {
	if w.logger == nil {
		// Resolve lazily so callers can stash a LogSink before Setup runs;
		// the global provider may install its real logger between
		// construction and the first write.
		w.logger = global.GetLoggerProvider().Logger("github.com/gesellix/bose-soundtouch")
	}

	msg := stripStdlibLogPrefix(line)
	if msg == "" {
		return
	}

	rec := otellog.Record{}
	rec.SetTimestamp(time.Now())
	rec.SetObservedTimestamp(time.Now())
	sev, sevText := inferSeverity(msg)
	rec.SetSeverity(sev)
	rec.SetSeverityText(sevText)
	rec.SetBody(otellog.StringValue(msg))

	w.logger.Emit(context.Background(), rec)
}

// stripStdlibLogPrefix removes the "2026/05/28 15:22:00 " prefix that
// log.LstdFlags (the default) prepends. Returns the original string when
// the prefix does not match — defensive in case callers customise flags.
func stripStdlibLogPrefix(s string) string {
	const prefixLen = len("2026/05/28 15:22:00 ")
	if len(s) >= prefixLen &&
		s[4] == '/' && s[7] == '/' && s[10] == ' ' &&
		s[13] == ':' && s[16] == ':' && s[19] == ' ' {
		return s[prefixLen:]
	}
	return s
}

// inferSeverity picks a level by sniffing the message prefix. Cheap and
// works well for this codebase's existing conventions ("Warning: ...",
// "Failed to ...", "[ERROR] ..."). When nothing matches we emit Info.
func inferSeverity(msg string) (otellog.Severity, string) {
	lower := strings.ToLower(msg)
	switch {
	case strings.HasPrefix(lower, "error"),
		strings.HasPrefix(lower, "[error]"),
		strings.HasPrefix(lower, "failed"),
		strings.HasPrefix(lower, "panic"):
		return otellog.SeverityError, "ERROR"
	case strings.HasPrefix(lower, "warn"),
		strings.HasPrefix(lower, "warning"),
		strings.HasPrefix(lower, "[warn]"):
		return otellog.SeverityWarn, "WARN"
	case strings.HasPrefix(lower, "debug"),
		strings.HasPrefix(lower, "[debug]"):
		return otellog.SeverityDebug, "DEBUG"
	default:
		return otellog.SeverityInfo, "INFO"
	}
}
