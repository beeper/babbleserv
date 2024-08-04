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

var BabbleservCommit = "dev"

const bootLogo = `

██████╗  █████╗ ██████╗ ██████╗ ██╗     ███████╗███████╗███████╗██████╗ ██╗   ██╗
██╔══██╗██╔══██╗██╔══██╗██╔══██╗██║     ██╔════╝██╔════╝██╔════╝██╔══██╗██║   ██║
██████╔╝███████║██████╔╝██████╔╝██║     █████╗  ███████╗█████╗  ██████╔╝██║   ██║
██╔══██╗██╔══██║██╔══██╗██╔══██╗██║     ██╔══╝  ╚════██║██╔══╝  ██╔══██╗╚██╗ ██╔╝
██████╔╝██║  ██║██████╔╝██████╔╝███████╗███████╗███████║███████╗██║  ██║ ╚████╔╝
╚═════╝ ╚═╝  ╚═╝╚═════╝ ╚═════╝ ╚══════╝╚══════╝╚══════╝╚══════╝╚═╝  ╚═╝  ╚═══╝

Let the babbling commence!
`

func main() {
	debug := flag.Bool("debug", false, "Display debug logs")
	trace := flag.Bool("trace", false, "Display trace logs")
	prettyLogs := flag.Bool("prettyLogs", false, "Display pretty logs")

	configFilename := flag.String("config", "config.yaml", "Config filename")

	routes := flag.Bool("routes", false, "Start routes")
	workers := flag.Bool("workers", false, "Start workers")

	flag.Parse()

	if *prettyLogs {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if *trace {
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
		log.Debug().Msg("Debug logging enabled")
		log.Trace().Msg("Trace logging enabled")
	} else if *debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
		log.Debug().Msg("Debug logging enabled")
	}

	log.Info().
		Str("commit", BabbleservCommit).
		Msg("Booting Babbleserv...")

	cfg := config.NewBabbleConfig(*configFilename, BabbleservCommit)

	if *routes {
		cfg.RoutesEnabled = true
	}
	if *workers {
		cfg.WorkersEnabled = true
	}

	babbleserv := internal.NewBabbleserv(cfg)

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	babbleserv.Start()

	if *prettyLogs {
		log.Info().Msg(bootLogo)
	} else {
		log.Info().Msg("Babbleserv booted, let the babbling commence!")
	}

	<-done
	babbleserv.Stop()

	log.Info().Msg("Babbleserv shutdown complete")
}
