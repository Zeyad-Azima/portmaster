package filterlists

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/safing/portbase/database"
	"github.com/safing/portbase/database/query"
	"github.com/safing/portbase/log"
	"github.com/safing/portbase/updater"
	"github.com/tevino/abool"
)

var updateInProgress = abool.New()

// tryListUpdate wraps performUpdate but ensures the module's
// error state is correctly set or resolved.
func tryListUpdate(ctx context.Context) error {
	err := performUpdate(ctx)

	if err != nil {
		if !isLoaded() {
			module.Error(filterlistsDisabled, err.Error())
		} else {
			module.Warning(filterlistsUpdateFailed, err.Error())
		}
		return err
	}

	// if the module is in an error, warning or hint state resolve that right now.
	module.Resolve(filterlistsDisabled)
	module.Resolve(filterlistsStaleDataSurvived)
	module.Resolve(filterlistsUpdateInProgress)
	return nil
}

func performUpdate(ctx context.Context) error {
	if !updateInProgress.SetToIf(false, true) {
		log.Debugf("intel/filterlists: upgrade already in progress")
		return nil
	}
	defer updateInProgress.UnSet()

	module.Hint(filterlistsUpdateInProgress, filterlistsUpdateInProgressDescr)

	upgradables, err := getUpgradableFiles()
	if err != nil {
		return err
	}
	log.Debugf("intel/filterlists: resources to update: %v", upgradables)

	if len(upgradables) == 0 {
		log.Debugf("intel/filterlists: ignoring update, latest version is already used")
		return nil
	}

	cleanupRequired := false
	filterToUpdate := defaultFilter

	// perform the actual upgrade by processing each file
	// in the returned order.
	for idx, file := range upgradables {
		log.Debugf("intel/filterlists: applying update (%d) %s version %s", idx, file.Identifier(), file.Version())

		if file == baseFile {
			if idx != 0 {
				log.Warningf("intel/filterlists: upgrade order is wrong, base file needs to be updated first not at idx %d", idx)
				// we still continue because after processing the base
				// file everything is correct again, we just used some
				// CPU and IO resources for nothing when processing
				// the previous files.
			}
			cleanupRequired = true

			// since we are processing a base update we will create our
			// bloom filters from scratch.
			filterToUpdate = newScopedBloom()
		}

		if err := processListFile(ctx, filterToUpdate, file); err != nil {
			return fmt.Errorf("failed to process upgrade %s: %w", file.Identifier(), err)
		}
	}

	if filterToUpdate != defaultFilter {
		// replace the bloom filters in our default
		// filter.
		defaultFilter.replaceWith(filterToUpdate)
	}

	// from now on, the database is ready and can be used if
	// it wasn't loaded yet.
	if !isLoaded() {
		close(filterListsLoaded)
	}

	if err := defaultFilter.saveToCache(); err != nil {
		// just handle the error by logging as it's only consequence
		// is that we will need to reprocess all files during the next
		// start.
		log.Errorf("intel/filterlists: failed to persist bloom filters in cache database: %s", err)
	}

	// if we processed the base file we need to perform
	// some cleanup on filterlists entities that have not
	// been updated now. Once we are done, start a worker
	// for that purpose.
	if cleanupRequired {
		if err := module.RunWorker("filterlists:cleanup", removeAllObsoleteFilterEntries); err != nil {
			// if we failed to remove all stale cache entries
			// we abort now WITHOUT updating the database version. This means
			// we'll try again during the next update.
			module.Warning(filterlistsStaleDataSurvived, filterlistsStaleDataDescr)
			return fmt.Errorf("failed to cleanup stale cache records: %w", err)
		}
	}

	// try to save the highest version of our files.
	highestVersion := upgradables[len(upgradables)-1]
	if err := setCacheDatabaseVersion(highestVersion.Version()); err != nil {
		log.Errorf("intel/filterlists: failed to save cache database version: %s", err)
	} else {
		log.Infof("intel/filterlists: successfully migrated cache database to %s", highestVersion.Version())
	}

	return nil
}

func removeAllObsoleteFilterEntries(ctx context.Context) error {
	log.Debugf("intel/filterlists: cleanup task started, removing obsolete filter list entries ...")
	n, err := cache.Purge(ctx, query.New(filterListKeyPrefix).Where(
		// TODO(ppacher): remember the timestamp we started the last update
		// and use that rather than "one hour ago"
		query.Where("UpdatedAt", query.LessThan, time.Now().Add(-time.Hour).Unix()),
	))
	if err != nil {
		return err
	}

	log.Debugf("intel/filterlists: successfully removed %d obsolete entries", n)
	return nil
}

// getUpgradableFiles returns a slice of filterlists files
// that should be updated. The files MUST be updated and
// processed in the returned order!
func getUpgradableFiles() ([]*updater.File, error) {
	var updateOrder []*updater.File

	cacheDBInUse := isLoaded()

	if baseFile == nil || baseFile.UpgradeAvailable() || !cacheDBInUse {
		var err error
		baseFile, err = getFile(baseListFilePath)
		if err != nil {
			return nil, err
		}
		log.Tracef("intel/filterlists: base file needs update, selected version %s", baseFile.Version())
		updateOrder = append(updateOrder, baseFile)
	}

	if intermediateFile == nil || intermediateFile.UpgradeAvailable() || !cacheDBInUse {
		var err error
		intermediateFile, err = getFile(intermediateListFilePath)
		if err != nil && err != updater.ErrNotFound {
			return nil, err
		}

		if err == nil {
			log.Tracef("intel/filterlists: intermediate file needs update, selected version %s", intermediateFile.Version())
			updateOrder = append(updateOrder, intermediateFile)
		}
	}

	if urgentFile == nil || urgentFile.UpgradeAvailable() || !cacheDBInUse {
		var err error
		urgentFile, err = getFile(urgentListFilePath)
		if err != nil && err != updater.ErrNotFound {
			return nil, err
		}

		if err == nil {
			log.Tracef("intel/filterlists: urgent file needs update, selected version %s", urgentFile.Version())
			updateOrder = append(updateOrder, urgentFile)
		}
	}

	return resolveUpdateOrder(updateOrder)
}

func resolveUpdateOrder(updateOrder []*updater.File) ([]*updater.File, error) {
	// sort the update order by ascending version
	sort.Sort(byAscVersion(updateOrder))
	log.Tracef("intel/filterlists: order of updates: %v", updateOrder)

	var cacheDBVersion *version.Version
	if !isLoaded() {
		cacheDBVersion, _ = version.NewSemver("v0.0.0")
	} else {
		var err error
		cacheDBVersion, err = getCacheDatabaseVersion()
		if err != nil {
			if err != database.ErrNotFound {
				log.Errorf("intel/filterlists: failed to get cache database version: %s", err)
			}
			cacheDBVersion, _ = version.NewSemver("v0.0.0")
		}
	}

	startAtIdx := -1
	for idx, file := range updateOrder {
		ver, _ := version.NewSemver(file.Version())
		log.Tracef("intel/filterlists: checking file with version %s against %s", ver, cacheDBVersion)
		if ver.GreaterThan(cacheDBVersion) && (startAtIdx == -1 || file == baseFile) {
			startAtIdx = idx
		}
	}

	// if startAtIdx == -1 we don't have any upgradables to
	// process.
	if startAtIdx == -1 {
		log.Tracef("intel/filterlists: nothing to process, latest version %s already in use", cacheDBVersion)
		return nil, nil
	}

	// skip any files that are lower then the current cache db version
	// or after which a base upgrade would be performed.
	return updateOrder[startAtIdx:], nil
}

type byAscVersion []*updater.File

func (fs byAscVersion) Len() int { return len(fs) }
func (fs byAscVersion) Less(i, j int) bool {
	vi, _ := version.NewSemver(fs[i].Version())
	vj, _ := version.NewSemver(fs[j].Version())

	return vi.LessThan(vj)
}
func (fs byAscVersion) Swap(i, j int) {
	fi := fs[i]
	fj := fs[j]

	fs[i] = fj
	fs[j] = fi
}
