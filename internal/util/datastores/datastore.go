package datastores

import (
	"context"
	"io"
	"time"

	"github.com/beeper/babbleserv/internal/config"
)

type ObjectInfo struct {
	Size        int64
	ContentType string
}

type DatastoreConfig struct {
	Locations []string
}

type Datastore interface {
	Key() string
	GetConfig() DatastoreConfig
	Configure(config.BabbleConfig, map[string]any)
	IsEnabled() bool

	GetObjectPresignedURL(context.Context, string, time.Duration) (string, error)
	PutObjectPresignedURL(context.Context, string, time.Duration) (string, error)

	GetObject(context.Context, string) (io.Reader, error)
	GetObjectInfo(context.Context, string) (ObjectInfo, error)
	PutObject(context.Context, string, io.Reader, ObjectInfo) error
}

type baseDatastore struct {
	locations []string
	enabled   bool
	key       string
}

func (d *baseDatastore) GetConfig() DatastoreConfig {
	return DatastoreConfig{
		Locations: d.locations,
	}
}

func (d *baseDatastore) IsEnabled() bool {
	return d.enabled
}

func (d *baseDatastore) Key() string {
	return d.key
}

func (d *baseDatastore) Configure(data map[string]any) {
	d.key = data["key"].(string)

	if locations, found := data["locations"]; found {
		if locationList, ok := locations.([]string); !ok {
			panic("invalid locations")
		} else {
			d.locations = locationList
		}
	}
	if enabled, found := data["enabled"]; found {
		if enabledBool, ok := enabled.(bool); !ok {
			panic("invalid bool")
		} else {
			d.enabled = enabledBool
		}
	}
}
