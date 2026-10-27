package config

import (
	"crypto/ed25519"
	"os"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
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
	TLSCertPath   string   `yaml:"tlsCertPath"`
	TLSKeyPath    string   `yaml:"tlsKeyPath"`
}

type keyConfig struct {
	Path             string `yaml:"path"`
	ExpiredTimestamp int64  `yaml:"expiredTimestamp"`
}

// What a publish's transaction may hold before the room record, leaving room under FoundationDB's
// 10 MB for the record and a send's extra writes
const PublishMandatoryMaxBytes = 9_000_000

// Bounds on the work of one transaction when sending events to a room
type stateBudgetConfig struct {
	// Approximate bytes staged per transaction, leaving headroom for FoundationDB overhead
	// Default: 5 MB
	StagingBytes int `yaml:"stagingBytes"`
	// Distinct contexts a state resolution run inline resolves, past which an artifact job runs it
	// Default: 64
	InlineResolutionContexts int `yaml:"inlineResolutionContexts"`
	// Candidate events of a state resolution run inline, past which an artifact job runs it
	// Default: 2000
	InlineResolutionCandidates int `yaml:"inlineResolutionCandidates"`
	// State tuples of the largest input of a state resolution run inline, past which an artifact job
	// runs it, as its auth difference reads the auth header of every state event of its inputs
	// Default: 10000
	InlineResolutionStateTuples int `yaml:"inlineResolutionStateTuples"`
}

// Bounds on a room's ingest job, which joins the room and publishes the events whose state it needs
type ingestConfig struct {
	// How long a job runs without progress before its lease expires and another runner may resume
	// it. The runner stops itself then too.
	// Default: 2m
	LeaseTimeout time.Duration `yaml:"leaseTimeout"`
	// How long after a join is enqueued its send_join keeps being sent again after failures that may
	// be transient, before the join fails
	// Default: 10m
	JoinTimeout time.Duration `yaml:"joinTimeout"`
	// Most events a join accepts from the resident server: state and auth chain events, or in a
	// state DAG room (MSC4242) the state DAG
	// Default: 1000000
	MaxJoinEvents int `yaml:"maxJoinEvents"`
	// Most PDUs, and their bytes, a room's job holds queued before /send waits for it to publish
	// some rather than queue more
	// Default: 2000 and 50000000
	MaxQueuedPDUs     int   `yaml:"maxQueuedPDUs"`
	MaxQueuedPDUBytes int64 `yaml:"maxQueuedPDUBytes"`
	// How long /send waits for a full queue before failing the transaction, which its sender sends
	// again later. Shorter than the minute Synapse waits for a response, so a sender gone meanwhile
	// leaves no request waiting.
	// Default: 50s
	QueueWait time.Duration `yaml:"queueWait"`
}

type babbleBotConfig struct {
	// Default: babblebot
	Localpart string `yaml:"localpart"`
	// Default: BabbleBot
	DisplayName string `yaml:"displayName"`
	// How long a join is pending before BabbleBot tells the joining user it is under way
	// Default: 60s
	JoinNoticeAfter time.Duration `yaml:"joinNoticeAfter"`
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
		Database       databaseConfig `yaml:"database"`
		Notifier       NotifierConfig `yaml:"notifier"`
		DefaultVersion string         `yaml:"defaultVersion"`

		// Offer the unstable state DAG room version (MSC4242): advertise it, create rooms of it, join
		// and take invites to them. Filling gaps in their state DAGs is not supported yet.
		UnstableStateDAGs bool `yaml:"unstableStateDAGs"`

		// Max notifications per user/room to keep count of, is not accurately applied, ie counts
		// may go over this before being trimmed back down after the timeout below or sufficient
		// traffic in the room.
		// Default: 100
		MaxNotificationsPerUserRoom int `yaml:"maxNotificationsPerUserRoom"`

		// Timeout after which we compact a rooms notifications even if less than max notifications
		// have been sent. This accounts for process restarts - the notification compactor stores
		// events sent per room in memory only.
		// Default: 3h
		CompactRoomNotificationsTimeout time.Duration `yaml:"compactRoomNotificationsTimeout"`

		// Most initial_state events a create room request may carry, all published with the create
		// event
		// Default: 100
		MaxCreateRoomInitialState int `yaml:"maxCreateRoomInitialState"`
		// Default: 1000
		MaxCreateRoomInvites int `yaml:"maxCreateRoomInvites"`

		StateBudget stateBudgetConfig `yaml:"stateBudget"`
		Ingest      ingestConfig      `yaml:"ingest"`
	} `yaml:"rooms"`

	Accounts struct {
		Database databaseConfig `yaml:"database"`
		Notifier NotifierConfig `yaml:"notifier"`

		RefreshAccessTokenExpire time.Duration `yaml:"refreshAccessTokenExpire"`

		// How long to keep device changes around (/keys/changes endpoint)
		DeviceChangesRetention time.Duration `yaml:"deviceChangesRetention"`

		// Allow public registration. If unset, registration is public only when no secret is configured.
		PublicRegistration *bool `yaml:"publicRegistration"`

		// Value of X-Babbleserv-Register-Secret that permits registration even when public registration is disabled.
		RegisterSecretHeaderValue string `yaml:"registerSecretHeaderValue"`
	} `yaml:"accounts"`

	Transient struct {
		Database databaseConfig `yaml:"database"`
		Notifier NotifierConfig `yaml:"notifier"`

		// How long until we time out online presence -> unavailable
		PresenceTimeout time.Duration `yaml:"presenceTimeout"`
		// Now often to check for timed out presence
		PresenceTimeoutCheckInterval time.Duration `yaml:"presenceTimeoutCheckInterval"`
	} `yaml:"transient"`

	Media struct {
		Enabled                    bool                      `yaml:"enabled"`
		Database                   databaseConfig            `yaml:"database"`
		Notifier                   NotifierConfig            `yaml:"notifier"`
		Datastores                 map[string]map[string]any `yaml:"datastores"`
		PresignedURLTimeout        time.Duration             `yaml:"presignedURLTimeout"`
		MaxUploadSize              int64                     `yaml:"maxUploadSize"` // Defaults to 50 MiB.
		MaxRemoteDownloadSize      int64                     `yaml:"maxRemoteDownloadSize"`
		RemoteDownloadTimeout      time.Duration             `yaml:"remoteDownloadTimeout"`
		AllowPrivateMediaRedirects bool                      `yaml:"allowPrivateMediaRedirects"`
		MaxThumbnailPixels         int64                     `yaml:"maxThumbnailPixels"`       // Defaults to 20 million pixels.
		MaxThumbnailSourcePixels   int64                     `yaml:"maxThumbnailSourcePixels"` // Defaults to 40 million pixels.
		PendingUploadTimeout       time.Duration             `yaml:"pendingUploadTimeout"`     // Defaults to 24 hours.
		MaxPendingUploadWait       time.Duration             `yaml:"maxPendingUploadWait"`     // Defaults to 1 minute.
	} `yaml:"media"`

	System struct {
		Database databaseConfig `yaml:"database"`
	} `yaml:"system"`

	Routes struct {
		Servers []serverConfig `yaml:"servers"`
	} `yaml:"routes"`

	// A server-owned local user telling local users about work on their behalf, such as slow joins
	BabbleBot babbleBotConfig `yaml:"babbleBot"`

	Federation struct {
		MaxFetchMissingEvents       int  `yaml:"maxFetchMissingEvents"`
		FetchProfileForMemberEvents bool `yaml:"fetchProfileForMemberEvents"`
		// Most state events send_join serves of a state DAG room (MSC4242). A room with more refuses
		// joins before taking them, rather than leave the joining server joined here alone.
		// Default: 1000000
		MaxServedStateDAG int `yaml:"maxServedStateDAG"`
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
		// Disable to-device txn ID deduplication
		DisableToDeviceTransactionIDCheck bool `yaml:"disableToDeviceTransactionIDs"`
		// Disable DeviceKeys & CrossSigningKeys equal check (ie always apply)
		DisableKeysEqualCheck bool `yaml:"disableKeysEqualCheck"`
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

	if cfg.Transient.PresenceTimeout == 0 {
		cfg.Transient.PresenceTimeout = time.Hour
	}
	if cfg.Transient.PresenceTimeoutCheckInterval == 0 {
		cfg.Transient.PresenceTimeoutCheckInterval = time.Minute
	}

	if cfg.Rooms.DefaultVersion == "" {
		cfg.Rooms.DefaultVersion = "11"
	}
	if cfg.Rooms.MaxNotificationsPerUserRoom == 0 {
		cfg.Rooms.MaxNotificationsPerUserRoom = 100
	}
	if cfg.Rooms.CompactRoomNotificationsTimeout == 0 {
		cfg.Rooms.CompactRoomNotificationsTimeout = 3 * time.Hour
	}
	if cfg.Rooms.MaxCreateRoomInitialState <= 0 {
		cfg.Rooms.MaxCreateRoomInitialState = 100
	}
	if cfg.Rooms.MaxCreateRoomInvites <= 0 {
		cfg.Rooms.MaxCreateRoomInvites = 1000
	}
	if cfg.Rooms.StateBudget.StagingBytes <= 0 {
		cfg.Rooms.StateBudget.StagingBytes = 5_000_000
	}
	if cfg.Rooms.StateBudget.InlineResolutionContexts <= 0 {
		cfg.Rooms.StateBudget.InlineResolutionContexts = 64
	}
	if cfg.Rooms.StateBudget.InlineResolutionCandidates <= 0 {
		cfg.Rooms.StateBudget.InlineResolutionCandidates = 2000
	}
	if cfg.Rooms.StateBudget.InlineResolutionStateTuples <= 0 {
		cfg.Rooms.StateBudget.InlineResolutionStateTuples = 10_000
	}
	if cfg.Rooms.Ingest.LeaseTimeout <= 0 {
		cfg.Rooms.Ingest.LeaseTimeout = 2 * time.Minute
	}
	if cfg.Rooms.Ingest.JoinTimeout <= 0 {
		cfg.Rooms.Ingest.JoinTimeout = 10 * time.Minute
	}
	if cfg.Rooms.Ingest.MaxJoinEvents <= 0 {
		cfg.Rooms.Ingest.MaxJoinEvents = 1_000_000
	}
	if cfg.Federation.MaxServedStateDAG <= 0 {
		cfg.Federation.MaxServedStateDAG = 1_000_000
	}
	if cfg.Rooms.Ingest.MaxQueuedPDUs <= 0 {
		cfg.Rooms.Ingest.MaxQueuedPDUs = 2000
	}
	if cfg.Rooms.Ingest.MaxQueuedPDUBytes <= 0 {
		cfg.Rooms.Ingest.MaxQueuedPDUBytes = 50_000_000
	}
	if cfg.Rooms.Ingest.QueueWait <= 0 {
		cfg.Rooms.Ingest.QueueWait = 50 * time.Second
	}
	if cfg.BabbleBot.Localpart == "" {
		cfg.BabbleBot.Localpart = "babblebot"
	}
	if cfg.BabbleBot.DisplayName == "" {
		cfg.BabbleBot.DisplayName = "BabbleBot"
	}
	if cfg.BabbleBot.JoinNoticeAfter <= 0 {
		cfg.BabbleBot.JoinNoticeAfter = time.Minute
	}

	return cfg
}

var stableRoomVersions = []string{"3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}

// RoomVersions returns the room versions this server creates rooms of and takes invites to, each
// with whether it is stable
func (c *BabbleConfig) RoomVersions() map[string]bool {
	versions := make(map[string]bool, len(stableRoomVersions)+1)
	for _, version := range stableRoomVersions {
		versions[version] = true
	}
	if c.Rooms.UnstableStateDAGs {
		versions[string(gomatrixserverlib.RoomVersionStateDAGs)] = false
	}
	return versions
}

// JoinsRoomVersion reports whether this server joins rooms of a version and takes invites to them:
// any gomatrixserverlib implements in the event format this server hashes, the unstable state DAG
// version only once enabled
func (c *BabbleConfig) JoinsRoomVersion(version string) bool {
	impl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersion(version))
	return err == nil && impl.EventFormat() == gomatrixserverlib.EventFormatV2 && (!impl.StateDAGs() || c.Rooms.UnstableStateDAGs)
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
