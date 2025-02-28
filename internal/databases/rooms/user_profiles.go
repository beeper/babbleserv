package rooms

import (
	"context"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
	"github.com/beeper/babbleserv/internal/util/lock"
)

func (r *RoomsDatabase) GetUserProfile(ctx context.Context, userID id.UserID) (*types.UserProfile, error) {
	return util.DoReadTransaction(ctx, r.db, func(txn fdb.ReadTransaction) (*types.UserProfile, error) {
		key := r.users.KeyForUserProfile(userID)
		b := txn.Get(key).MustGet()
		if b == nil {
			return nil, nil
		}
		return types.MustNewUserProfileFromBytes(b), nil
	})
}

const updateUserProfileLockPrefix = "UpdateUserProfile:"

func (r *RoomsDatabase) UpdateUserProfile(ctx context.Context, userID id.UserID, key string, value any) error {
	// Per the (somewhat ridiculous) profiles spec, here we must update the profile and then send an
	// event to *every room the user is joined in* to update the in-room membership state event:
	// https://spec.matrix.org/v1.10/client-server-api/#events-on-change-of-profile-information

	// We do the membership event updates in a background goroutine after the profile update. We
	// grab the memberships *at time of profile update*.
	var profile *types.UserProfile
	memberships, err := util.DoWriteTransaction(ctx, r.db, func(txn fdb.Transaction) (types.Memberships, error) {
		profileKey := r.users.KeyForUserProfile(userID)
		b := txn.Get(profileKey).MustGet()
		if b == nil {
			profile = &types.UserProfile{}
		} else {
			profile = types.MustNewUserProfileFromBytes(b)
		}

		switch key {
		case "displayname":
			if profile.DisplayName == value {
				return nil, types.ErrProfileNotChanged
			}
			profile.DisplayName = value.(string)
		case "avatar_url":
			if profile.AvatarURL == value {
				return nil, types.ErrProfileNotChanged
			}
			profile.AvatarURL = value.(string)
		default:
			profile.Custom[key] = value
		}

		txn.Set(profileKey, profile.ToMsgpack())

		return r.users.TxnLookupUserMemberships(txn, userID)
	})
	if err != nil {
		return err
	}

	// Now we have our memberships, kick off a background task to generate the member event updates.
	backgroundCtx := zerolog.Ctx(ctx).With().
		Str("background_task", "SendMemberEventsAfterProfileUpdate").
		Logger().
		WithContext(context.Background())
	log := zerolog.Ctx(backgroundCtx)

	r.backgroundWg.Add(1)
	go func() {
		defer r.backgroundWg.Done()

		// Take a lock on updating the users profile so parallel requests don't clobber each other.
		lockKey := updateUserProfileLockPrefix + userID.String()
		lock := lock.GetLock(backgroundCtx, r, lockKey, lock.LockOptions{
			Timeout: 10 * time.Second, // we refresh this every event send
		})
		defer lock.Release()

		var updated int
		for roomID, membershipTup := range memberships {
			if membershipTup.Membership != event.MembershipJoin {
				continue
			}

			content := profile.ToMembershipContent()
			content["membership"] = membershipTup.Membership
			sKey := string(userID)

			partialEv := types.NewPartialEvent(roomID, event.StateMember, &sKey, userID, content)

			results, err := r.SendLocalEvents(
				backgroundCtx,
				roomID,
				[]*types.PartialEvent{partialEv},
				SendLocalEventsOptions{
					TxnRefresh: lock.TxnRefresh,
				},
			)
			if err != nil {
				log.Err(err).Msg("Failed to send updated member event")
			} else if len(results.Rejected) > 0 {
				err := results.Rejected[0].Error
				log.Warn().Err(err).Msg("Updated member event not allowed")
			} else {
				updated++
			}
		}

		log.Info().Int("updated", updated).Msg("Send updated member events")
	}()

	return nil
}
