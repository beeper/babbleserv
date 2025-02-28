package config

import (
	"crypto/ed25519"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type databaseConfig struct {
	ClusterFilePath       string `yaml:"clusterFilePath"`
	TransactionTimeout    int64  `yaml:"transactionTimeout"`
	TransactionRetryLimit int64  `yaml:"transactionRetryLimit"`
}

type serverConfig struct {
	ListenAddr    string   `yaml:"listenAddr"`
	ServiceGroups []string `yaml:"serviceGroups"`
}

type keyConfig struct {
	Path             string `yaml:"path"`
	ExpiredTimestamp int64  `yaml:"expiredTimestamp"`
}

type NotifierConfig struct {
	RedisAddr    string `yaml:"redisAddr"`
	RedisChannel string `yaml:"redisChannel"`
}

type BabbleConfig struct {
	ServerName string `yaml:"serverName"`

	SigningKeys               map[string]keyConfig `yaml:"signingKeys"`
	SigningKeyRefreshInterval time.Duration        `yaml:"signingKeyRefreshInterval"`

	Rooms struct {
		Enabled        bool           `yaml:"enabled"`
		Database       databaseConfig `yaml:"database"`
		Notifier       NotifierConfig `yaml:"notifier"`
		DefaultVersion string         `yaml:"defaultVersion"`
	} `yaml:"rooms"`

	Accounts struct {
		Enabled  bool           `yaml:"enabled"`
		Database databaseConfig `yaml:"database"`
		Notifier NotifierConfig `yaml:"notifier"`

		RefreshAccessTokenExpire time.Duration `yaml:"refreshAccessTokenExpire"`
	}

	Transient struct {
		Enabled  bool           `yaml:"enabled"`
		Database databaseConfig `yaml:"database"`
		Notifier NotifierConfig `yaml:"notifier"`
	}

	Media struct {
		Enabled             bool                      `yaml:"enabled"`
		Database            databaseConfig            `yaml:"database"`
		Notifier            NotifierConfig            `yaml:"notifier"`
		Datastores          map[string]map[string]any `yaml:"datastores"`
		PresignedURLTimeout time.Duration             `yaml:"presignedURLTimeout"`
	}

	Routes struct {
		Servers []serverConfig `yaml:"servers"`
	} `yaml:"routes"`

	Workers struct {
	} `yaml:"workers"`

	Federation struct {
		MaxFetchMissingEvents int `yaml:"maxFetchMissingEvents"`
	} `yaml:"federation"`

	// For development usage - serve the .well-known client/server endpoints
	WellKnown struct {
		Server string `yaml:"server"`
		Client string `yaml:"client"`
	} `yaml:"wellKnown"`

	SecretSwitches struct {
		// Allow sending create events over federation transactions, should NOT
		// be enabled in production, but very useful for testing.
		EnableFederatedSendRoomCreate bool `yaml:"enableFederatedSendRoomCreate"`
		// Create datastore buckets that don't exist
		AutoCreateDatastoreBuckets bool `yaml:"autoCreateDatastoreBuckets"`
	} `yaml:"secretSwitches"`

	// Provided via caller (added at build time)
	UserAgent      string `yaml:"-"`
	RoutesEnabled  bool   `yaml:"-"`
	WorkersEnabled bool   `yaml:"-"`

	// Internal cache
	activeSigningKeyID string                        `yaml:"-"`
	signingKeyCache    map[string]ed25519.PrivateKey `yaml:"-"`
}

func NewBabbleConfig(filename string, commitHash string) BabbleConfig {
	data, err := os.ReadFile(filename)
	if err != nil {
		panic(err)
	}

	var cfg BabbleConfig
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		panic(err)
	}

	cfg.UserAgent = "Babbleserv (" + commitHash + ")"

	var hasActiveKey bool
	for keyID, key := range cfg.SigningKeys {
		if key.ExpiredTimestamp == 0 {
			if hasActiveKey {
				panic("cannot have more than one active key")
			}
			hasActiveKey = true
			cfg.activeSigningKeyID = keyID
		}
	}

	cfg.signingKeyCache = make(map[string]ed25519.PrivateKey, len(cfg.SigningKeys))

	if cfg.SigningKeyRefreshInterval == 0 {
		cfg.SigningKeyRefreshInterval = time.Hour
	}

	return cfg
}

func (c *BabbleConfig) MustGetSigningKey(keyID string) ed25519.PrivateKey {
	if key, found := c.signingKeyCache[keyID]; found {
		return key
	}

	keyConfig, found := c.SigningKeys[keyID]
	if !found {
		panic("signing key not found")
	}
	data, err := os.ReadFile(keyConfig.Path)
	if err != nil {
		panic(err)
	}

	key := ed25519.PrivateKey(data)
	c.signingKeyCache[keyID] = key
	return key
}

func (c *BabbleConfig) MustGetActiveSigningKey() (string, ed25519.PrivateKey) {
	return c.activeSigningKeyID, c.MustGetSigningKey(c.activeSigningKeyID)
}
