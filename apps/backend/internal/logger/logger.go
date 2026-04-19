package logger

import (
	"os"
	"time"

	"github.com/rs/zerolog"
)

var Log = zerolog.New(os.Stdout).With().Timestamp().Logger()

// Init updates the global Log to a pretty or JSON logger based on env,
// then returns it. Call this once at startup after config is loaded.
func Init(env string) zerolog.Logger {
	Log = New(env)
	return Log
}

func New(env string) zerolog.Logger {
	if env == "development" {
		return zerolog.New(zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}).With().Timestamp().Caller().Logger()
	}

	return zerolog.New(os.Stdout).With().Timestamp().Logger()
}
