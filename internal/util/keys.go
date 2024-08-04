package util

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
)

type serverKeys struct {
	validUntil time.Time
	verifyKeys map[string]ed25519.PublicKey
}

type KeyStore struct {
	fclient fclient.FederationClient
	cache   map[string]serverKeys
}

func NewKeyStore(fclient fclient.FederationClient) *KeyStore {
	return &KeyStore{
		fclient: fclient,
		cache:   make(map[string]serverKeys),
	}
}

func (k *KeyStore) GetServerKeys(ctx context.Context, serverName string) (serverKeys, error) {
	cached, found := k.cache[serverName]
	if found && cached.validUntil.After(time.Now()) {
		return cached, nil
	}

	zerolog.Ctx(ctx).Info().
		Str("server", serverName).
		Msg("Fetching keys from server")

	keys, err := k.fclient.GetServerKeys(ctx, spec.ServerName(serverName))
	if err != nil {
		// TODO: fallback to querying other, allowed, servers here if we can't get keys directly
		return cached, err
	}

	// Convert gomatrixserverlib unncessary types -> stdlib types
	verifyKeys := make(map[string]ed25519.PublicKey)
	for keyID, key := range keys.VerifyKeys {
		verifyKeys[string(keyID)] = ed25519.PublicKey(key.Key)
	}

	serverKeys := serverKeys{
		validUntil: keys.ValidUntilTS.Time(),
		verifyKeys: verifyKeys,
	}

	k.cache[serverName] = serverKeys
	return serverKeys, nil
}

func (k *KeyStore) VerifyJSONFromServer(ctx context.Context, serverName string, b []byte) error {
	serverKeys, err := k.GetServerKeys(ctx, serverName)
	if err != nil {
		return err
	}

	for keyID, key := range serverKeys.verifyKeys {
		if err = VerifyJSON(b, serverName, keyID, key); err == nil {
			return nil
		} else {
			zerolog.Ctx(ctx).Trace().
				Err(err).
				Str("key_id", keyID).
				Str("key", Base64Encode(key)).
				Str("server_name", serverName).
				Str("bytes", string(b)).
				Msg("JSON signature failed")
		}
	}

	return errors.New("invalid signature")
}

func (k *KeyStore) VerifyHistoricalJSONFromServer(
	ctx context.Context,
	serverName string,
	at time.Time,
	b []byte,
) error {
	return errors.New("not implemented: VerifyHistoricalJSONFromServer")
}
