//go:build !windows

package hostbroker

import "context"

func OpenPlatformState() (CredentialStore, Journal, error) {
	return nil, nil, ErrUnsupported
}

func OpenPlatformCredentialStore() (CredentialStore, error) { return nil, ErrUnsupported }

func OpenPlatformJournal() (Journal, error) { return nil, ErrUnsupported }

func OpenPlatformConfigStore() (ConfigStore, error) { return nil, ErrUnsupported }

func AcquirePlatformLock() (Unlock, error) { return nil, ErrUnsupported }

func ManageAutostart(context.Context, ScheduleAction, string, bool) (SchedulePlan, error) {
	return SchedulePlan{}, ErrUnsupported
}
