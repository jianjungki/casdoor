// Copyright 2021 The Casdoor Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package object

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/casdoor/casdoor/util"
	"github.com/robfig/cron/v3"
)

var (
	// cronMap is keyed by the full syncer id (owner/name) so that syncers with
	// the same name under different owners/organizations do not collide.
	cronMap map[string]*cron.Cron

	// syncersMu guards against overlapping runs of the same syncer.
	// In Kubernetes, pod restarts / network blips can otherwise cause a second
	// run of a still-running (and possibly hung) sync to pile up, which shows
	// up as "sync timeout". Each syncer is only ever allowed a single run.
	syncersMu     map[string]*sync.Mutex
	syncersMuLock sync.Mutex
)

// defaultSyncTimeout caps how long a single periodic sync run may take, so a
// slow/hung upstream (e.g. an API endpoint that never responds, or a stalled
// database query) cannot hold the cron job forever.
const defaultSyncTimeout = 10 * time.Minute

func init() {
	cronMap = map[string]*cron.Cron{}
	syncersMu = map[string]*sync.Mutex{}
}

// syncerID returns the stable identity used to key cron jobs and run-mutexes.
func syncerID(syncer *Syncer) string {
	return syncer.Owner + "/" + syncer.Name
}

func getCronMap(id string) *cron.Cron {
	m, ok := cronMap[id]
	if !ok {
		m = cron.New(cron.WithChain(cron.SkipIfStillRunning(cron.DefaultLogger)))
		cronMap[id] = m
	}
	return m
}

func getSyncerRunMutex(id string) *sync.Mutex {
	syncersMuLock.Lock()
	defer syncersMuLock.Unlock()

	m, ok := syncersMu[id]
	if !ok {
		m = &sync.Mutex{}
		syncersMu[id] = m
	}
	return m
}

func clearCron(id string) {
	c, ok := cronMap[id]
	if ok {
		// Do NOT block on <-ctx.Done() here. cron.Stop() waits for any
		// currently running job to finish; if that job is hung on a slow
		// upstream, waiting would deadlock syncer updates / deletion.
		c.Stop()
		delete(cronMap, id)
	}
}

// runSyncerWithTimeout executes a syncer run within a fixed deadline and
// guarantees no two runs of the same syncer execute concurrently, regardless
// of how they were triggered (cron, manual run, or update).
func runSyncerWithTimeout(syncer *Syncer, kind string, fn func() error) {
	id := syncerID(syncer)
	mu := getSyncerRunMutex(id)

	// Non-blocking acquisition: if a previous run is still in progress
	// (possibly hung), drop this run rather than piling up goroutines.
	if !mu.TryLock() {
		fmt.Printf("[syncer: %s] %s skipped: previous run still in progress\n", id, kind)
		return
	}
	defer mu.Unlock()

	startedAt := time.Now()
	fmt.Printf("[syncer: %s] %s started\n", id, kind)
	defer func() {
		fmt.Printf("[syncer: %s] %s finished in %s\n", id, kind, time.Since(startedAt).Round(time.Millisecond))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), defaultSyncTimeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		err := fn()
		if err != nil {
			fmt.Printf("[syncer: %s] %s error: %s\n", id, kind, err.Error())
		}
	}()

	select {
	case <-done:
		return
	case <-ctx.Done():
		// Do NOT release the lock here: the underlying fn goroutine may still be
		// running. We log the timeout for visibility but keep waiting on done so
		// the per-syncer lock stays held until the run truly finishes, which
		// prevents a new run from piling up on top of a hung one.
		fmt.Printf("[syncer: %s] %s timed out after %s, waiting for in-flight run to finish\n", id, kind, defaultSyncTimeout)
		<-done
	}
}

// runSyncerAsync queues a syncer run and returns immediately. The existing
// per-syncer mutex in runSyncerWithTimeout prevents duplicate runs when an API
// request, an initial run, and a cron tick happen at the same time.
func runSyncerAsync(syncer *Syncer, kind string, fn func() error) {
	fmt.Printf("[syncer: %s] %s queued\n", syncerID(syncer), kind)
	util.SafeGoroutine(func() {
		runSyncerWithTimeout(syncer, kind, fn)
	})
}

func addSyncerJob(syncer *Syncer) error {
	id := syncerID(syncer)
	if id == "/" {
		return fmt.Errorf("syncer owner and name must not be empty")
	}

	deleteSyncerJob(syncer)

	if !syncer.IsEnabled {
		return nil
	}

	err := syncer.initAdapter()
	if err != nil {
		return err
	}

	// Queue the initial sync instead of blocking the add/update HTTP request.
	// Errors are logged by runSyncerWithTimeout and do not prevent scheduling.
	runSyncerAsync(syncer, "initial", func() error {
		if err := syncer.syncUsers(); err != nil {
			return err
		}
		return syncer.syncGroups()
	})

	if syncer.SyncInterval <= 0 {
		fmt.Printf("[syncer: %s] SyncInterval=%d is invalid (<=0), skipping periodic scheduling (one-shot sync already ran)\n", id, syncer.SyncInterval)
		return nil
	}

	schedule := fmt.Sprintf("@every %ds", syncer.SyncInterval)
	cron := getCronMap(id)
	_, err = cron.AddFunc(schedule, func() {
		runSyncerWithTimeout(syncer, "cron", func() error {
			syncer.syncUsers()
			return syncer.syncGroups()
		})
	})
	if err != nil {
		return err
	}

	cron.Start()
	return nil
}

func deleteSyncerJob(syncer *Syncer) {
	clearCron(syncerID(syncer))
	// Close any open connections when deleting the job
	_ = syncer.Close()
}
