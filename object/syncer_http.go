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
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// syncerHttpTimeoutEnv is the environment variable that controls the HTTP
// timeout used by all API-based syncers (WeCom, DingTalk, Lark, Okta, Azure AD,
// SCIM, ...). It is interpreted in SECONDS. When unset or invalid, the default
// (30s) is used.
const syncerHttpTimeoutEnv = "CASDOOR_SYNCER_HTTP_TIMEOUT"

const defaultSyncerHttpTimeout = 30 * time.Second

var (
	syncerHttpTimeout     time.Duration
	syncerHttpTimeoutOnce sync.Once
)

// getSyncerHttpTimeout returns the configured HTTP timeout for API syncers.
// It is read once from the environment and cached for the life of the process.
func getSyncerHttpTimeout() time.Duration {
	syncerHttpTimeoutOnce.Do(func() {
		syncerHttpTimeout = defaultSyncerHttpTimeout
		if raw := os.Getenv(syncerHttpTimeoutEnv); raw != "" {
			secs, err := strconv.Atoi(raw)
			if err == nil && secs > 0 {
				syncerHttpTimeout = time.Duration(secs) * time.Second
			}
		}
	})
	return syncerHttpTimeout
}

// newSyncerHttpClient returns an *http.Client whose timeout honors the
// configured syncer HTTP timeout environment variable.
func newSyncerHttpClient() *http.Client {
	return &http.Client{Timeout: getSyncerHttpTimeout()}
}

// syncerHttpContext returns a context with the configured syncer HTTP timeout.
// Callers must invoke the returned cancel function.
func syncerHttpContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), getSyncerHttpTimeout())
}
