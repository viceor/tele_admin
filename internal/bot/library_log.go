package bot

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Library logging policy (MYC-83, MYC-92).
//
// The go-telegram/bot library writes to the standard log package by default.
// Its default errors handler and default update handler print raw update
// payloads (message text, user data) at all times, and its debug mode prints
// every request body and webhook body. None of that may reach the logs, and
// log_level=debug must not switch it on. The bot therefore:
//
//   - never enables tgbot.WithDebug (enforced by the test in library_log_test.go);
//   - routes library errors through newLibraryErrorsHandler, which logs only
//     whitelisted fields: a stable operation label, a fixed error category and
//     the numeric fields of typed errors (retry_after, migrate_to_chat_id).
//     Server-provided text, raw payloads and cause strings are never logged;
//   - routes library debug events through newLibraryDebugHandler, which logs
//     only the constant format string and never its arguments;
//   - routes unmatched updates through newLibraryDefaultHandler, which logs
//     the update ID only.

// libraryErrorAttrs reduces a library error to whitelisted log fields: a
// stable operation label and a slice of category and numeric attributes.
//
// The library formats wrapped errors as "<fixed prefix>, <dynamic part>",
// where the dynamic part can be a raw update payload or a server-provided
// Description, so only the prefix before the first ", " is kept. Typed errors
// embed dynamic text in their Error string (TooManyRequestsError and
// MigrateError put the server Description into Message), so their whitelisted
// numeric fields are extracted via errors.As instead of any text parsing.
// Errors without a ", " separator are treated as untrusted text and reduced
// to a generic operation label. The error string itself is never logged.
func libraryErrorAttrs(err error) (op string, attrs []any) {
	op = "telegram library error"
	if i := strings.Index(err.Error(), ", "); i >= 0 {
		op = err.Error()[:i]
	}
	var tooMany *tgbot.TooManyRequestsError
	if errors.As(err, &tooMany) {
		return op, []any{"category", "too_many_requests", "retry_after", tooMany.RetryAfter}
	}
	var migrate *tgbot.MigrateError
	if errors.As(err, &migrate) {
		return op, []any{"category", "migrate", "migrate_to_chat_id", migrate.MigrateToChatID}
	}
	return op, []any{"category", libraryErrorCategory(err)}
}

// libraryErrorCategory maps sentinel errors wrapped by the library to fixed
// category names. It never derives text from the error message itself.
func libraryErrorCategory(err error) string {
	switch {
	case errors.Is(err, tgbot.ErrorForbidden):
		return "forbidden"
	case errors.Is(err, tgbot.ErrorBadRequest):
		return "bad_request"
	case errors.Is(err, tgbot.ErrorUnauthorized):
		return "unauthorized"
	case errors.Is(err, tgbot.ErrorNotFound):
		return "not_found"
	case errors.Is(err, tgbot.ErrorConflict):
		return "conflict"
	default:
		return "other"
	}
}

// newLibraryErrorsHandler returns the tgbot.ErrorsHandler used by the bot.
// Only whitelisted fields are logged: op, category and numeric attributes
// extracted from typed errors. No error or cause string is ever written.
func newLibraryErrorsHandler(log *slog.Logger) tgbot.ErrorsHandler {
	return func(err error) {
		if err == nil {
			return
		}
		op, attrs := libraryErrorAttrs(err)
		args := make([]any, 0, 2+len(attrs))
		args = append(args, "op", op)
		args = append(args, attrs...)
		log.Error("telegram library error", args...)
	}
}

// newLibraryDebugHandler returns the tgbot.DebugHandler used by the bot. The
// library only calls it when debug mode is on, which the bot never enables.
// Even if it were enabled, only the constant format string is logged, so
// request and response payloads are never written.
func newLibraryDebugHandler(log *slog.Logger) tgbot.DebugHandler {
	return func(format string, _ ...any) {
		log.Debug("telegram library debug event", "event", format)
	}
}

// newLibraryDefaultHandler returns the tgbot.HandlerFunc for updates that no
// registered handler matches. It logs identifiers only, never the update body.
func newLibraryDefaultHandler(log *slog.Logger) tgbot.HandlerFunc {
	return func(_ context.Context, _ *tgbot.Bot, upd *models.Update) {
		if upd == nil {
			return
		}
		log.Debug("unmatched telegram update", "update_id", upd.ID)
	}
}
