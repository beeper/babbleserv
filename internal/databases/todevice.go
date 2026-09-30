package databases

import (
	"context"

	"github.com/beeper/babbleserv/internal/databases/transient"
	"github.com/beeper/babbleserv/internal/types"
)

// Essentially a wrapper around Transient.SendRawToDeviceEvents that expands any events for local
// users with device ID "*" via Accounts.GetUserDevices.
func (d *Databases) SendToDeviceEvents(
	ctx context.Context,
	tds []*types.ToDevice,
	options transient.SendToDeviceOptions,
) (*transient.SendToDeviceResults, error) {
	expandedTDs := make([]*types.ToDevice, 0, len(tds)*2)

	for _, td := range tds {
		if td.DeviceID == "*" && td.UserID.Homeserver() == d.config.ServerName {
			devices, err := d.Accounts.GetUserDevices(ctx, td.UserID)
			if err != nil {
				return nil, err
			}
			for _, d := range devices {
				tdCopy := *td
				tdCopy.DeviceID = d.ID
				expandedTDs = append(expandedTDs, &tdCopy)
			}
		} else {
			expandedTDs = append(expandedTDs, td)
		}
	}

	return d.Transient.SendRawToDeviceEvents(ctx, expandedTDs, options)
}
