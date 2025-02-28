package util

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util/datastores"
)

type Datastores struct {
	stores              map[string]datastores.Datastore
	presignedURLTimeout time.Duration
}

func NewDatastores(config config.BabbleConfig) *Datastores {
	stores := make(map[string]datastores.Datastore)

	for key, storeConfig := range config.Media.Datastores {
		sType, ok := storeConfig["type"].(string)
		if !ok {
			panic(fmt.Errorf("invalid store type: %v", sType))
		}
		storeConfig["key"] = key
		switch sType {
		case "s3":
			store := datastores.NewS3Store()
			store.Configure(config, storeConfig)
			stores[key] = store
		}
	}

	return &Datastores{
		stores:              stores,
		presignedURLTimeout: config.Media.PresignedURLTimeout,
	}
}

func (d *Datastores) GetDatastore(name string) datastores.Datastore {
	return d.stores[name]
}

func (d *Datastores) MustGetDatastore(name string) datastores.Datastore {
	ds := d.GetDatastore(name)
	if ds == nil {
		panic(fmt.Errorf("no datastore found: %s", name))
	}
	return ds
}

func (d *Datastores) PickDatastoreByLocation(locationHint string) datastores.Datastore {
	for _, ds := range d.stores {
		if ds.IsEnabled() && slices.Contains(ds.GetConfig().Locations, locationHint) {
			return ds
		}
	}
	return nil
}

func (d *Datastores) PickDatastoreForRequest(r *http.Request) datastores.Datastore {
	locationHint := r.URL.Query().Get("location")
	if locationHint != "" {
		return d.PickDatastoreByLocation(locationHint)
	}

	// Use remote addr?

	// Default: just pick the first enabled store
	for _, ds := range d.stores {
		if ds.IsEnabled() {
			return ds
		}
	}

	return nil
}

func (d *Datastores) PresignedGetURLForMedia(ctx context.Context, m *types.Media) (string, error) {
	store := d.GetDatastore(m.StoreKey)
	return store.GetObjectPresignedURL(ctx, m.StorePath, d.presignedURLTimeout)
}

func (d *Datastores) PresignedPutURLForMedia(ctx context.Context, m *types.Media) (string, error) {
	store := d.GetDatastore(m.StoreKey)
	return store.PutObjectPresignedURL(ctx, m.StorePath, d.presignedURLTimeout)
}

func (d *Datastores) GetObjectInfoForMedia(ctx context.Context, m *types.Media) (datastores.ObjectInfo, error) {
	store := d.GetDatastore(m.StoreKey)
	return store.GetObjectInfo(ctx, m.StorePath)
}

func (d *Datastores) GetObjectForMedia(ctx context.Context, m *types.Media) (io.Reader, error) {
	store := d.GetDatastore(m.StoreKey)
	return store.GetObject(ctx, m.StorePath)
}

func (d *Datastores) PutObjectForMedia(ctx context.Context, m *types.Media, input io.Reader) error {
	store := d.GetDatastore(m.StoreKey)
	return store.PutObject(ctx, m.StorePath, input, datastores.ObjectInfo{})
}
