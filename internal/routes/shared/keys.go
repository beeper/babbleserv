package shared

import (
	"context"
	"maps"
	"slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/config"
	"github.com/beeper/babbleserv/internal/databases"
)

type MissingUserKeyClaimsByServer map[string]map[id.UserID]map[id.DeviceID]id.KeyAlgorithm

func ClaimUserKeys(
	ctx context.Context,
	config config.BabbleConfig,
	db *databases.Databases,
	req mautrix.OneTimeKeysRequest,
) (mautrix.RespClaimKeys, MissingUserKeyClaimsByServer, error) {
	resp := mautrix.RespClaimKeys{
		OneTimeKeys: make(map[id.UserID]map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey),
	}

	serverToUserDevices := make(MissingUserKeyClaimsByServer, len(req))

	for userID, deviceIDToAlgorithm := range req {
		// Build per server remote claim requests
		if userID.Homeserver() != config.ServerName {
			if _, ok := serverToUserDevices[userID.Homeserver()]; !ok {
				serverToUserDevices[userID.Homeserver()] = make(map[id.UserID]map[id.DeviceID]id.KeyAlgorithm, 1)
			}
			if _, ok := serverToUserDevices[userID.Homeserver()][userID]; !ok {
				serverToUserDevices[userID.Homeserver()][userID] = make(map[id.DeviceID]id.KeyAlgorithm, 1)
			}
			serverToUserDevices[userID.Homeserver()][userID] = deviceIDToAlgorithm
			continue
		}

		// Claim keys for local users
		for deviceID, algorithm := range deviceIDToAlgorithm {
			keys, err := db.Accounts.ClaimOrGetPreKeys(ctx, userID, deviceID, algorithm, 1)
			if err != nil {
				return resp, nil, err
			} else if keys == nil {
				continue
			}
			if _, ok := resp.OneTimeKeys[userID]; !ok {
				resp.OneTimeKeys[userID] = make(map[id.DeviceID]map[id.KeyID]mautrix.OneTimeKey, 1)
			}
			resp.OneTimeKeys[userID][deviceID] = keys
		}
	}

	return resp, serverToUserDevices, nil
}

type MissingUserKeysByServer map[string]map[id.UserID]mautrix.DeviceIDList

func GetUserKeys(
	ctx context.Context,
	config config.BabbleConfig,
	db *databases.Databases,
	req mautrix.DeviceKeysRequest,
	requestUserID id.UserID,
) (mautrix.RespQueryKeys, MissingUserKeysByServer, error) {
	resp := mautrix.RespQueryKeys{
		DeviceKeys:      make(map[id.UserID]map[id.DeviceID]mautrix.DeviceKeys),
		MasterKeys:      make(map[id.UserID]mautrix.CrossSigningKeys),
		SelfSigningKeys: make(map[id.UserID]mautrix.CrossSigningKeys),
		UserSigningKeys: make(map[id.UserID]mautrix.CrossSigningKeys),
	}

	// Handle local users, collect remote users queries (by server)
	serverToUserDevices := make(map[string]map[id.UserID]mautrix.DeviceIDList)

	for userID, deviceIDs := range req {
		// TODO: we should cache these
		if server := userID.Homeserver(); server != config.ServerName {
			if _, ok := serverToUserDevices[server]; !ok {
				serverToUserDevices[server] = make(map[id.UserID]mautrix.DeviceIDList, 1)
			}
			serverToUserDevices[server][userID] = deviceIDs
			continue
		}

		// Get local user data
		xsKeys, err := db.Accounts.GetUserCrossSigningKeys(ctx, userID, requestUserID)
		if err != nil {
			return resp, serverToUserDevices, err
		} else if xsKeys != nil {
			if xsKeys.Master.KeyID() != "" {
				resp.MasterKeys[userID] = xsKeys.Master.CrossSigningKeys
			}
			if xsKeys.SelfSigning.KeyID() != "" {
				resp.SelfSigningKeys[userID] = xsKeys.SelfSigning.CrossSigningKeys
			}
			if xsKeys.UserSigning.KeyID() != "" {
				resp.UserSigningKeys[userID] = xsKeys.UserSigning.CrossSigningKeys
			}
		}

		devices, err := db.Accounts.GetUserDevices(ctx, userID)
		if err != nil {
			return resp, serverToUserDevices, err
		}
		deviceIDToName := make(map[id.DeviceID]string, len(devices))
		for _, d := range devices {
			deviceIDToName[d.ID] = d.DisplayName
		}

		if len(deviceIDs) == 0 {
			// Empty list = all known deviceIDs
			deviceIDs = slices.Collect(maps.Keys(deviceIDToName))
		} else {
			// Filter input list for known deviceIDs
			existingDeviceIDs := make(mautrix.DeviceIDList, len(devices))
			for _, did := range deviceIDs {
				if _, ok := deviceIDToName[did]; ok {
					existingDeviceIDs = append(existingDeviceIDs, did)
				}
			}
			deviceIDs = existingDeviceIDs
		}

		// Always create the userID key, even if we have no results
		if _, ok := resp.DeviceKeys[userID]; !ok {
			resp.DeviceKeys[userID] = make(map[id.DeviceID]mautrix.DeviceKeys)
		}

		// This could be optimized with a GetManyDeviceKeys method/txn
		for _, did := range deviceIDs {
			deviceKeys, err := db.Accounts.GetDeviceKeys(ctx, userID, did, requestUserID)
			if err != nil {
				return resp, serverToUserDevices, err
			} else if deviceKeys != nil {
				if deviceKeys.Unsigned == nil {
					deviceKeys.Unsigned = make(map[string]any, 1)
				}
				if deviceName := deviceIDToName[did]; deviceName != "" {
					deviceKeys.Unsigned["device_display_name"] = deviceName
				}
				resp.DeviceKeys[userID][did] = *deviceKeys
			}
		}
	}

	return resp, serverToUserDevices, nil
}
