package process

import (
	"context"

	"github.com/safing/portbase/log"
	"github.com/safing/portmaster/profile"
)

// GetProfile finds and assigns a profile set to the process.
func (p *Process) GetProfile(ctx context.Context) (changed bool, err error) {
	// Update profile metadata outside of *Process lock.
	var localProfile *profile.Profile
	defer p.updateProfileMetadata(localProfile)

	p.Lock()
	defer p.Unlock()

	// Check if profile is already loaded.
	if p.profile != nil {
		log.Tracer(ctx).Trace("process: profile already loaded")
		return
	}

	// If not, continue with loading the profile.
	log.Tracer(ctx).Trace("process: loading profile")

	// Check if we need a special profile.
	profileID := ""
	switch p.Pid {
	case UnidentifiedProcessID:
		profileID = profile.UnidentifiedProfileID
	case SystemProcessID:
		profileID = profile.SystemProfileID
	}

	// Get the (linked) local profile.
	localProfile, err = profile.GetProfile(profile.SourceLocal, profileID, p.Path)
	if err != nil {
		return false, err
	}

	// Assign profile to process.
	p.LocalProfileKey = localProfile.Key()
	p.profile = localProfile.LayeredProfile()

	return true, nil
}

func (p *Process) updateProfileMetadata(localProfile *profile.Profile) {
	// Check if there is a profile to work with.
	if localProfile == nil {
		return
	}

	// Update metadata of profile.
	metadataUpdated := localProfile.UpdateMetadata(p.Name)

	// Mark profile as used.
	profileChanged := localProfile.MarkUsed()

	// Save the profile if we changed something.
	if metadataUpdated || profileChanged {
		err := localProfile.Save()
		if err != nil {
			log.Warningf("process: failed to save profile %s: %s", localProfile.ScopedID(), err)
		}
	}
}
