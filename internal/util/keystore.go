package util

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"
)

type serverKeys struct {
	validUntil time.Time
	verifyKeys map[string]ed25519.PublicKey
}

type serverKeysError struct {
	validUntil time.Time
	err        error
}

type KeyStore struct {
	fclient  fclient.FederationClient
	cache    map[string]serverKeys
	errCache map[string]serverKeysError
	lock     sync.Mutex
}

func NewKeyStore(fclient fclient.FederationClient) *KeyStore {
	return &KeyStore{
		fclient:  fclient,
		cache:    make(map[string]serverKeys),
		errCache: make(map[string]serverKeysError),
	}
}

func (k *KeyStore) GetServerKeys(ctx context.Context, serverName string) (serverKeys, error) {
	k.lock.Lock()
	defer k.lock.Unlock()

	if cached, found := k.cache[serverName]; found && cached.validUntil.After(time.Now()) {
		return cached, nil
	}

	if cachedErr, found := k.errCache[serverName]; found && cachedErr.validUntil.After(time.Now()) {
		return serverKeys{}, cachedErr.err
	}

	zerolog.Ctx(ctx).Info().
		Str("server2", serverName).
		Msg("Fetching keys from server")

	keys, err := k.fclient.GetServerKeys(ctx, spec.ServerName(serverName))
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get server keys")
		k.errCache[serverName] = serverKeysError{
			err:        err,
			validUntil: time.Now().Add(time.Minute),
		}
		// TODO: fallback to querying other, allowed, servers here if we can't get keys directly
		return serverKeys{}, err
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

	errs := make([]error, 0)

	// Accept signatures under the full host:port key and just host
	serverNames := []string{serverName}
	if strings.Contains(serverName, ":") {
		serverNames = append(serverNames, strings.Split(serverName, ":")[0])
	}

	for keyID, key := range serverKeys.verifyKeys {
		for _, sName := range serverNames {
			if err = VerifyJSON(b, sName, keyID, key); err == nil {
				return nil
			} else {
				errs = append(errs, err)
				zerolog.Ctx(ctx).Trace().
					Err(err).
					Str("key_id", keyID).
					Str("key", Base64Encode(key)).
					Str("server_name", sName).
					Str("bytes", string(b)).
					Msg("JSON signature failed")
			}
		}
	}

	return errors.Join(errs...)
}
