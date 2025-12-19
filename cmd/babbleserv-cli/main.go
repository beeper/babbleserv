package main

import (
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"os"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
)

func main() {
	configFilename := flag.String("config", "config.yaml", "Config filename")

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	ctxLog := log.With().Caller().Str("component", "default_context_logger").Logger()
	zerolog.DefaultContextLogger = &ctxLog

	if len(os.Args) < 2 {
		log.Error().Msg("Invalid number of arguments (should be one or more).")
		os.Exit(1)
	}

	arg := os.Args[1]

	switch arg {
	case "generate-signing-key":
		cmd := flag.NewFlagSet("", flag.ExitOnError)
		filename := cmd.String("out", "key.ed25519", "Output filename")
		cmd.Parse(os.Args[2:])

		_, privateKey, err := ed25519.GenerateKey(nil)
		if err != nil {
			panic(err)
		}

		if err := os.WriteFile(*filename, privateKey, 0600); err != nil {
			panic(err)
		}

		log.Info().Msgf("Written: %s", *filename)

	case "test-federation":
		cfg := config.NewBabbleConfig(*configFilename, "")
		keyID, key := cfg.MustGetActiveSigningKey()

		serverName := spec.ServerName(cfg.ServerName)

		client := fclient.NewFederationClient([]*fclient.SigningIdentity{{
			ServerName: serverName,
			KeyID:      gomatrixserverlib.KeyID(keyID),
			PrivateKey: key,
		}})

		// keys, err := client.GetServerKeys(context.TODO(), serverName)
		// log.Info().Err(err).Any("keys", keys).Msg("keys resp")

		// res, err := client.GetEvent(
		// 	context.TODO(),
		// 	serverName,
		// 	spec.ServerName("beeper.com"),
		// 	"$CCwKeLPbtjVcJ5YGj6U2mwWiic_zfUde3gRFZ0AYy0k",
		// )

		// res, err := client.MakeJoin(
		// 	context.TODO(),
		// 	serverName,
		// 	spec.ServerName("beeper-dev.com"),
		// 	"!EWgNwoVcfGifZxAjsv:beeper-dev.com",
		// 	"@nick:babbleserv-dev.fizzadar.com",
		// )
		// log.Info().Err(err).Any("res", res).Msg("Resp")

		log.Info().Msg("HI")
		res, err := client.GetEvent(context.Background(), serverName, spec.ServerName("beeper-staging.com"), "$IzOtzXQhcXuySclQtmQkKNtTi1Jl1DdBp3RrFoQ11kA")

		log.Info().Err(err).Any("RES", res).Msg("YES")

	case "generate-device-keys":
		cmd := flag.NewFlagSet("", flag.ExitOnError)
		userIDStr := cmd.String("userid", "", "User ID")
		deviceIDStr := cmd.String("deviceid", "", "Device ID")
		cmd.Parse(os.Args[2:])

		userID := id.UserID(*userIDStr)
		deviceID := id.DeviceID(*deviceIDStr)

		keys := GetDeviceKeys(userID, deviceID)
		log.Info().Any("DeviceKeys", keys).Msg("")

	case "generate-cross-signing-keys":
		cmd := flag.NewFlagSet("", flag.ExitOnError)
		userIDStr := cmd.String("userid", "", "User ID")
		deviceIDStr := cmd.String("deviceid", "", "Device ID")
		cmd.Parse(os.Args[2:])

		userID := id.UserID(*userIDStr)
		deviceID := id.DeviceID(*deviceIDStr)

		keys := GetCrossSigningKeys(userID, deviceID)
		log.Info().Any("CrossSigningKeys", keys).Msg("")

	default:
		log.Error().Msg(fmt.Sprintf("Invalid argument: %s", arg))
		os.Exit(1)
	}
}
