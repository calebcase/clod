package main

import (
	"context"
	"io"
	stdlog "log"
	"os"

	"github.com/alecthomas/kong"
	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
)

func Run() (err error) {
	ctx := context.Background()

	cli := &CLI{}

	kctx := kong.Parse(
		cli,
		kong.Bind(&ctx),
		kong.Bind(&cli.Flags),
	)

	zerolog.SetGlobalLevel(cli.Flags.Log.Level)

	var output io.Writer = os.Stderr

	switch cli.Flags.Log.Format {
	case "json":
		// Default JSON output
	case "console":
		output = zerolog.ConsoleWriter{
			Out:     output,
			NoColor: !isatty.IsTerminal(os.Stderr.Fd()),
		}
	}

	logger := zerolog.New(output).With().Timestamp().Logger()

	stdlog.SetFlags(0)
	stdlog.SetOutput(logger)

	kctx.Bind(logger)

	err = kctx.Run()
	if err != nil {
		logger.Error().Err(err).Msgf("error: %+v", err)
		return err
	}

	return nil
}

func main() {
	err := Run()
	if err != nil {
		os.Exit(1)
	}
}
