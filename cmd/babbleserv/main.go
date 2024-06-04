package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/beeper/babbleserv/internal"
	"github.com/beeper/babbleserv/internal/config"
)

func main() {
	debug := flag.Bool("debug", false, "Display debug logs")
	trace := flag.Bool("trace", false, "Display trace logs")
	prettyLogs := flag.Bool("prettyLogs", false, "Display pretty logs")
	configFilename := flag.String("config", "config.yaml", "Config filename")
	flag.Parse()

	if *prettyLogs {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if *debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
		log.Debug().Msg("Debug logging enabled")
	}
	if *trace {
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
		log.Trace().Msg("Trace logging enabled")
	}

	log.Debug().
		Msg("Starting Babbleserv")

	cfg := config.NewBabbleConfig(*configFilename)
	babbleserv := internal.NewBabbleserv(cfg)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	log.Info().Msg("Starting syncserv...")
	babbleserv.Start()

	<-done
	babbleserv.Stop()
}
