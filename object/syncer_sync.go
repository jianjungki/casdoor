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
	"fmt"
	"time"
)

func (syncer *Syncer) syncUsers() (err error) {
	if len(syncer.TableColumns) == 0 {
		return fmt.Errorf("The syncer table columns should not be empty")
	}

	start := time.Now()
	fmt.Printf("[syncer: %s/%s] syncUsers() started (%d table columns)\n", syncer.Owner, syncer.Name, len(syncer.TableColumns))
	defer func() {
		status := "OK"
		if err != nil {
			status = fmt.Sprintf("ERROR: %v", err)
		} else {
			// Persist last successful sync time so operators can detect a
			// syncer that silently stopped syncing via a stale last_sync.
			_ = updateSyncerLastSync(syncer)
		}
		fmt.Printf("[syncer: %s/%s] syncUsers() finished in %s, status=%s\n", syncer.Owner, syncer.Name, time.Since(start).Round(time.Millisecond), status)
	}()

	users, err := GetUsers(syncer.Organization)
	if err != nil {
		return err
	}

	oUsers, err := syncer.getOriginalUsers()
	if err != nil {
		return err
	}

	fmt.Printf("[syncer: %s/%s] local users=%d, upstream users=%d\n", syncer.Owner, syncer.Name, len(users), len(oUsers))

	var affiliationMap map[int]string
	if syncer.AffiliationTable != "" {
		_, affiliationMap, err = syncer.getAffiliationMap()
		if err != nil {
			return err
		}
	}

	key := syncer.getLocalPrimaryKey()

	// Build the local user map while detecting problems (duplicate or empty key
	// values) that would otherwise silently drop/merge users.
	myUsers := map[string]*User{}
	for _, m := range users {
		k := syncer.getUserValue(m, key)
		if k == "" || key == "id" && k == m.Id && m.Id == "" {
			fmt.Printf("[syncer: %s/%s][WARN] local user '%s' has empty primary key '%s', skipping to avoid silent corruption\n", syncer.Owner, syncer.Name, m.Name, key)
			continue
		}
		if pre, dup := myUsers[k]; dup {
			fmt.Printf("[syncer: %s/%s][WARN] DUPLICATE local primary key '%s'=%q used by users '%s' and '%s', last one wins - possible silent data loss\n",
				syncer.Owner, syncer.Name, key, k, pre.Name, m.Name)
		}
		myUsers[k] = m
	}

	// Same detection for the upstream (original) users map.
	myOUsers := map[string]*User{}
	for _, m := range oUsers {
		k := syncer.getUserValue(m, key)
		if k == "" {
			fmt.Printf("[syncer: %s/%s][WARN] upstream user '%s' has empty primary key '%s', skipping to avoid silent corruption\n", syncer.Owner, syncer.Name, m.Name, key)
			continue
		}
		if pre, dup := myOUsers[k]; dup {
			fmt.Printf("[syncer: %s/%s][WARN] DUPLICATE upstream primary key '%s'=%q used by users '%s' and '%s', last one wins - possible silent data loss\n",
				syncer.Owner, syncer.Name, key, k, pre.Name, m.Name)
		}
		myOUsers[k] = m
	}

	newUsers := []*User{}
	for _, oUser := range oUsers {
		primary := syncer.getUserValue(oUser, key)

		if _, ok := myUsers[primary]; !ok {
			newUser := syncer.createUserFromOriginalUser(oUser, affiliationMap)
			fmt.Printf("New user: %v\n", newUser)
			newUsers = append(newUsers, newUser)
		} else {
			user := myUsers[primary]
			oHash := syncer.calculateHash(oUser)
			if user.Hash == user.PreHash {
				if user.Hash != oHash {
					updatedUser := syncer.createUserFromOriginalUser(oUser, affiliationMap)
					updatedUser.Hash = oHash
					updatedUser.PreHash = oHash

					fmt.Printf("Update from oUser to user: %v\n", updatedUser)
					_, err = syncer.updateUserForOriginalFields(updatedUser, key)
					if err != nil {
						return err
					}
				}
			} else {
				if user.PreHash == oHash {
					if !syncer.IsReadOnly {
						updatedOUser := syncer.createOriginalUserFromUser(user)

						fmt.Printf("Update from user to oUser: %v\n", updatedOUser)
						_, err = syncer.updateUser(updatedOUser)
						if err != nil {
							return err
						}
					}

					// update preHash
					user.PreHash = user.Hash
					_, err = SetUserField(user, "pre_hash", user.PreHash)
					if err != nil {
						return err
					}
				} else {
					if user.Hash == oHash {
						// update preHash
						user.PreHash = user.Hash
						_, err = SetUserField(user, "pre_hash", user.PreHash)
						if err != nil {
							return err
						}
					} else {
						// True two-way conflict: the local user was modified
						// (user.Hash != user.PreHash), upstream was modified too
						// (user.PreHash != oHash), and they disagree
						// (user.Hash != oHash). The code below silently
						// overwrites the local change with the upstream value.
						// Surface a loud warning so this never happens silently.
						fmt.Printf("[syncer: %s/%s][CONFLICT] two-way modification conflict on key '%s'=%q for user '%s': local hash differs from upstream, OVERWRITING local changes with upstream source of truth\n",
							syncer.Owner, syncer.Name, key, primary, user.Name)

						updatedUser := syncer.createUserFromOriginalUser(oUser, affiliationMap)
						updatedUser.Hash = oHash
						updatedUser.PreHash = oHash

						fmt.Printf("Update from oUser to user (2nd condition, conflict - local overwritten): %v\n", updatedUser)
						_, err = syncer.updateUserForOriginalFields(updatedUser, key)
						if err != nil {
							return err
						}
					}
				}
			}
		}
	}

	if len(newUsers) != 0 {
		_, err = AddUsersInBatch(newUsers)
		if err != nil {
			return err
		}

		// Trigger webhooks for syncer user additions
		for _, newUser := range newUsers {
			TriggerWebhookForUser("new-user-syncer", newUser)
		}
	}

	if !syncer.IsReadOnly {
		for _, user := range users {
			primary := syncer.getUserValue(user, key)
			if _, ok := myOUsers[primary]; !ok {
				newOUser := syncer.createOriginalUserFromUser(user)

				fmt.Printf("New oUser: %v\n", newOUser)
				_, err = syncer.addUser(newOUser)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (syncer *Syncer) syncUsersNoError() {
	err := syncer.syncUsers()
	if err != nil {
		recordSyncerError(syncer, err)
		fmt.Printf("syncUsersNoError() error: %s\n", err.Error())
	}
}
