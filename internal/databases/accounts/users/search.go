package users

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
)

const (
	maxSearchDisplayNameRunes = 255
	maxSearchDisplayNameBytes = 1024
)

func validateSearchDisplayName(displayName string) error {
	if len(displayName) > maxSearchDisplayNameBytes || utf8.RuneCountInString(displayName) > maxSearchDisplayNameRunes {
		return fmt.Errorf("%w: maximum is %d Unicode code points and %d UTF-8 bytes", types.ErrProfileDisplayNameTooLong, maxSearchDisplayNameRunes, maxSearchDisplayNameBytes)
	}
	return nil
}

func searchGrams(values ...string) map[string]struct{} {
	grams := make(map[string]struct{})
	for _, value := range values {
		runes := []rune(strings.ToLower(value))
		for start := range runes {
			for width := 1; width <= 3 && start+width <= len(runes); width++ {
				grams[string(runes[start:start+width])] = struct{}{}
			}
		}
	}
	return grams
}

func (u *UsersDirectory) txnUpdateSearchIndex(txn fdb.Transaction, userID id.UserID, previous, current string) {
	oldGrams := searchGrams(userID.String(), previous)
	newGrams := searchGrams(userID.String(), current)
	for gram := range oldGrams {
		if _, exists := newGrams[gram]; !exists {
			txn.Clear(u.searchGrams.Pack(tuple.Tuple{gram, userID.String()}))
		}
	}
	for gram := range newGrams {
		if _, exists := oldGrams[gram]; !exists {
			txn.Set(u.searchGrams.Pack(tuple.Tuple{gram, userID.String()}), nil)
		}
	}
}

func userDirectoryRank(candidate *types.UserDirectoryCandidate, term string) int {
	userID := strings.ToLower(candidate.UserID.String())
	localpart := strings.ToLower(candidate.UserID.Localpart())
	displayName := strings.ToLower(candidate.DisplayName)
	if userID == term || localpart == term || displayName == term {
		return 0
	}
	if strings.HasPrefix(localpart, term) || strings.HasPrefix(displayName, term) {
		return 1
	}
	return 2
}

func (u *UsersDirectory) TxnSearchUserDirectory(
	txn fdb.ReadTransaction,
	searchTerm string,
	maxResults int,
	maxIndexScan int,
) ([]*types.UserDirectoryCandidate, bool, error) {
	term := strings.ToLower(searchTerm)
	termRunes := []rune(term)
	if len(termRunes) == 0 || maxResults <= 0 || maxIndexScan <= 0 {
		return []*types.UserDirectoryCandidate{}, false, nil
	}
	gram := string(termRunes[:min(3, len(termRunes))])
	kvs := txn.GetRange(u.searchGrams.Sub(gram), fdb.RangeOptions{
		Limit: maxIndexScan + 1, Mode: fdb.StreamingModeExact,
	}).GetSliceOrPanic()
	limited := len(kvs) > maxIndexScan
	if limited {
		kvs = kvs[:maxIndexScan]
	}
	userIDs := make([]id.UserID, len(kvs))
	profiles := make([]fdb.FutureByteSlice, len(kvs))
	for i, kv := range kvs {
		key, err := u.searchGrams.Unpack(kv.Key)
		if err != nil {
			return nil, false, err
		}
		userIDs[i] = id.UserID(key[1].(string))
		profiles[i] = txn.Get(u.keyForProfile(userIDs[i]))
	}
	results := make([]*types.UserDirectoryCandidate, 0, min(maxResults, len(kvs)))
	for i, userID := range userIDs {
		candidate := &types.UserDirectoryCandidate{UserID: userID}
		if b := profiles[i].MustGet(); b != nil {
			profile, err := types.NewUserProfileFromBytes(b)
			if err != nil {
				return nil, false, err
			}
			candidate.DisplayName, candidate.AvatarURL = profile.DisplayName, profile.AvatarURL
		}
		if strings.Contains(strings.ToLower(userID.String()), term) || strings.Contains(strings.ToLower(candidate.DisplayName), term) {
			results = append(results, candidate)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		left, right := results[i], results[j]
		leftRank, rightRank := userDirectoryRank(left, term), userDirectoryRank(right, term)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		leftProfile := left.DisplayName != "" || left.AvatarURL != ""
		rightProfile := right.DisplayName != "" || right.AvatarURL != ""
		if leftProfile != rightProfile {
			return leftProfile
		}
		return left.UserID < right.UserID
	})
	if len(results) > maxResults {
		results = results[:maxResults]
		limited = true
	}
	return results, limited, nil
}
