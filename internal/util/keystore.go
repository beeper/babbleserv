package util

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/rs/zerolog"

	"github.com/beeper/babbleserv/internal/util/signedjson"
)

type serverKeys struct {
	validUntil time.Time
	verifyKeys map[string]ed25519.PublicKey
}

type serverKeysError struct {
	validUntil time.Time
	err        error
}

// KeyStore caches servers' signing keys, fetching each server's at most once at a time. A fetch
// holds up only the callers needing that server's keys.
type KeyStore struct {
	fclient  fclient.FederationClient
	lock     sync.Mutex
	cache    map[string]serverKeys
	errCache map[string]serverKeysError
	fetches  map[string]*keyFetch
}

type keyFetch struct {
	done chan struct{}
	keys serverKeys
	err  error
}

func NewKeyStore(fclient fclient.FederationClient) *KeyStore {
	return &KeyStore{
		fclient:  fclient,
		cache:    make(map[string]serverKeys),
		errCache: make(map[string]serverKeysError),
		fetches:  make(map[string]*keyFetch),
	}
}

func (k *KeyStore) GetServerKeys(ctx context.Context, serverName string) (serverKeys, error) {
	keys, fetched, err := k.getServerKeys(ctx, serverName)
	if fetched {
		// A fetch can take seconds, a dead server the request's whole timeout
		ReportProgress(ctx)
	}
	return keys, err
}

// getServerKeys returns the server's cached keys, else waits for a fetch of them, starting one unless
// another caller has. The fetch carries on should the caller give up, for the others waiting on it.
func (k *KeyStore) getServerKeys(ctx context.Context, serverName string) (serverKeys, bool, error) {
	k.lock.Lock()
	if cached, found := k.cache[serverName]; found && cached.validUntil.After(time.Now()) {
		k.lock.Unlock()
		return cached, false, nil
	} else if cachedErr, found := k.errCache[serverName]; found && cachedErr.validUntil.After(time.Now()) {
		k.lock.Unlock()
		return serverKeys{}, false, cachedErr.err
	}
	fetch, found := k.fetches[serverName]
	if !found {
		fetch = &keyFetch{done: make(chan struct{})}
		k.fetches[serverName] = fetch
		go k.fetch(context.WithoutCancel(ctx), serverName, fetch)
	}
	k.lock.Unlock()

	select {
	case <-fetch.done:
		return fetch.keys, true, fetch.err
	case <-ctx.Done():
		return serverKeys{}, false, ctx.Err()
	}
}

func (k *KeyStore) fetch(ctx context.Context, serverName string, fetch *keyFetch) {
	keys, err := k.fetchServerKeys(ctx, serverName)
	k.lock.Lock()
	defer k.lock.Unlock()
	delete(k.fetches, serverName)
	if err != nil {
		k.errCache[serverName] = serverKeysError{
			err:        err,
			validUntil: time.Now().Add(time.Minute),
		}
	} else {
		k.cache[serverName] = keys
	}
	fetch.keys, fetch.err = keys, err
	close(fetch.done)
}

func (k *KeyStore) fetchServerKeys(ctx context.Context, serverName string) (_ serverKeys, err error) {
	defer RecoverPanic(zerolog.Ctx(ctx), &err)
	zerolog.Ctx(ctx).Info().
		Str("server2", serverName).
		Msg("Fetching keys from server")

	keys, err := k.fclient.GetServerKeys(ctx, spec.ServerName(serverName))
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get server keys")
		// TODO: fallback to querying other, allowed, servers here if we can't get keys directly
		return serverKeys{}, err
	}

	// Convert gomatrixserverlib unncessary types -> stdlib types
	verifyKeys := make(map[string]ed25519.PublicKey)
	for keyID, key := range keys.VerifyKeys {
		verifyKeys[string(keyID)] = ed25519.PublicKey(key.Key)
	}
	return serverKeys{
		validUntil: keys.ValidUntilTS.Time(),
		verifyKeys: verifyKeys,
	}, nil
}

func (k *KeyStore) VerifyJSONFromServer(ctx context.Context, serverName string, b []byte) error {
	serverKeys, err := k.GetServerKeys(ctx, serverName)
	if err != nil {
		return err
	}

	if len(serverKeys.verifyKeys) == 0 {
		return errors.New("server has no verification keys")
	}

	errs := make([]error, 0)

	// Accept signatures under the full host:port key and just host
	serverNames := []string{serverName}
	if strings.Contains(serverName, ":") {
		serverNames = append(serverNames, strings.Split(serverName, ":")[0])
	}

	for keyID, key := range serverKeys.verifyKeys {
		for _, sName := range serverNames {
			if err = signedjson.Verify(b, sName, keyID, key); err == nil {
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

func (k *KeyStore) VerifyJSONs(ctx context.Context, requests []gomatrixserverlib.VerifyJSONRequest) ([]gomatrixserverlib.VerifyJSONResult, error) {
	results := make([]gomatrixserverlib.VerifyJSONResult, len(requests))
	for i, request := range requests {
		serverName := string(request.ServerName)
		keys, err := k.GetServerKeys(ctx, serverName)
		if err != nil {
			results[i].Error = err
			continue
		}
		if request.ValidityCheckingFunc != nil && !request.ValidityCheckingFunc(request.AtTS, spec.AsTimestamp(keys.validUntil)) {
			results[i].Error = errors.New("server signing key was not valid at the event timestamp")
			continue
		}
		results[i].Error = errors.New("no valid signature from " + serverName)
		for keyID, key := range keys.verifyKeys {
			if signedjson.Verify(request.Message, serverName, keyID, key) == nil {
				results[i].Error = nil
				break
			}
		}
	}
	return results, nil
}
