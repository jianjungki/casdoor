// Copyright 2023 The Casdoor Authors. All Rights Reserved.
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

package sync

import (
	"log"
	"sync"
	"time"
)

const (
	minReconnectBackoff = time.Second
	maxReconnectBackoff = 30 * time.Second
)

func startSyncJob(db1 *Database, db2 *Database) error {
	var wg sync.WaitGroup
	wg.Add(2)

	// start canal1 replication (db1 -> db2)
	go func() {
		defer wg.Done()
		runCanalWithRetry(db1, db2)
	}()

	// start canal2 replication (db2 -> db1)
	go func() {
		defer wg.Done()
		runCanalWithRetry(db2, db1)
	}()

	wg.Wait()
	return nil
}

// runCanalWithRetry runs binlog replication from src to dst and automatically
// reconnects with exponential backoff when the connection is interrupted.
//
// In Kubernetes, network policies, DNS changes and pod restarts frequently
// drop long-lived binlog connections. Previously a single dropped connection
// returned an error and panicked (crashing the process), or silently never
// recovered — both of which manifest as "sync never completes / timeout".
func runCanalWithRetry(src *Database, dst *Database) {
	backoff := minReconnectBackoff

	for {
		runOnce := func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("casdoor: canal replication %s -> %s panicked: %v", src.database, dst.database, r)
				}
			}()

			c, err := src.startCanal(dst)
			if err != nil {
				log.Printf("casdoor: failed to start canal %s -> %s: %v", src.database, dst.database, err)
				return
			}
			defer c.Close()

			log.Printf("casdoor: canal replication started %s -> %s", src.database, dst.database)
			// Run blocks until the binlog stream breaks (network error, master
			// restart, replication error, ...) and then returns.
			err = c.Run()
			if err != nil {
				log.Printf("casdoor: canal replication %s -> %s stopped with error: %v", src.database, dst.database, err)
			} else {
				log.Printf("casdoor: canal replication %s -> %s stopped", src.database, dst.database)
			}
		}

		runOnce()

		// Back off before reconnecting to avoid hammering a flaky master.
		time.Sleep(backoff)
		if backoff < maxReconnectBackoff {
			backoff *= 2
			if backoff > maxReconnectBackoff {
				backoff = maxReconnectBackoff
			}
		}
	}
}
