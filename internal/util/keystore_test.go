package util_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/fclient"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/beeper/babbleserv/internal/util"
)

// slowKeysClient answers key requests once a server's release channel, if any, is closed
type slowKeysClient struct {
	fclient.FederationClient
	mu      sync.Mutex
	fetches map[spec.ServerName]int
	release map[spec.ServerName]chan struct{}
}

func (c *slowKeysClient) GetServerKeys(_ context.Context, serverName spec.ServerName) (gomatrixserverlib.ServerKeys, error) {
	c.mu.Lock()
	c.fetches[serverName]++
	release := c.release[serverName]
	c.mu.Unlock()
	if release != nil {
		<-release
	}
	var keys gomatrixserverlib.ServerKeys
	keys.ValidUntilTS = spec.AsTimestamp(time.Now().Add(time.Hour))
	return keys, nil
}

func (c *slowKeysClient) fetchCount(serverName spec.ServerName) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fetches[serverName]
}

func TestKeyStoreFetchesEachServerOnce(t *testing.T) {
	client := &slowKeysClient{
		fetches: make(map[spec.ServerName]int),
		release: map[spec.ServerName]chan struct{}{"dead.example": make(chan struct{})},
	}
	keyStore := util.NewKeyStore(client)

	var progress atomic.Int32
	ctx := util.WithProgress(context.Background(), func() { progress.Add(1) })
	var waiters sync.WaitGroup
	for range 2 {
		waiters.Go(func() {
			_, err := keyStore.GetServerKeys(ctx, "dead.example")
			assert.NoError(t, err)
		})
	}
	require.Eventually(t, func() bool { return client.fetchCount("dead.example") == 1 }, 5*time.Second, time.Millisecond)

	quick, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := keyStore.GetServerKeys(quick, "live.example")
	require.NoError(t, err, "a fetch from another server does not wait on a slow one")

	gaveUp, cancelGiveUp := context.WithCancel(context.Background())
	cancelGiveUp()
	_, err = keyStore.GetServerKeys(gaveUp, "dead.example")
	assert.ErrorIs(t, err, context.Canceled, "a caller giving up does not wait for the fetch")

	close(client.release["dead.example"])
	waiters.Wait()
	assert.Equal(t, 1, client.fetchCount("dead.example"), "callers wait on the fetch in flight")
	assert.Equal(t, int32(2), progress.Load(), "each caller that waited on the fetch reports progress")

	_, err = keyStore.GetServerKeys(ctx, "dead.example")
	require.NoError(t, err)
	assert.Equal(t, 1, client.fetchCount("dead.example"), "then the keys are cached")
	assert.Equal(t, int32(2), progress.Load())
}

// Giving up waiting for keys fails the whole verification, not each signature as if invalid
func TestVerifyJSONsFailsWithItsContext(t *testing.T) {
	client := &slowKeysClient{
		fetches: make(map[spec.ServerName]int),
		release: map[spec.ServerName]chan struct{}{"dead.example": make(chan struct{})},
	}
	defer close(client.release["dead.example"])
	keyStore := util.NewKeyStore(client)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	results, err := keyStore.VerifyJSONs(ctx, []gomatrixserverlib.VerifyJSONRequest{{ServerName: "dead.example", Message: []byte(`{}`)}})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, results)
}
