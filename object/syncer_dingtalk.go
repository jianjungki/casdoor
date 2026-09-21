// Copyright 2026 The Casdoor Authors. All Rights Reserved.
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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/casdoor/casdoor/util"
)

const (
	dingtalkMaxRetries     = 5
	dingtalkRetryBaseDelay = time.Second
)

// DingtalkSyncerProvider implements SyncerProvider for DingTalk API-based syncers
type DingtalkSyncerProvider struct {
	Syncer *Syncer
}

// InitAdapter initializes the DingTalk syncer (no database adapter needed)
func (p *DingtalkSyncerProvider) InitAdapter() error {
	// DingTalk syncer doesn't need database adapter
	return nil
}

// GetOriginalUsers retrieves all users from DingTalk API
func (p *DingtalkSyncerProvider) GetOriginalUsers() ([]*OriginalUser, error) {
	return p.getDingtalkUsers()
}

// AddUser adds a new user to DingTalk (not supported for read-only API)
func (p *DingtalkSyncerProvider) AddUser(user *OriginalUser) (bool, error) {
	// DingTalk syncer is typically read-only
	return false, fmt.Errorf("adding users to DingTalk is not supported")
}

// UpdateUser updates an existing user in DingTalk (not supported for read-only API)
func (p *DingtalkSyncerProvider) UpdateUser(user *OriginalUser) (bool, error) {
	// DingTalk syncer is typically read-only
	return false, fmt.Errorf("updating users in DingTalk is not supported")
}

// TestConnection tests the DingTalk API connection
func (p *DingtalkSyncerProvider) TestConnection() error {
	_, err := p.getDingtalkAccessToken()
	return err
}

// Close closes any open connections (no-op for DingTalk API-based syncer)
func (p *DingtalkSyncerProvider) Close() error {
	// DingTalk syncer doesn't maintain persistent connections
	return nil
}

type DingtalkAccessTokenResp struct {
	Errcode     int    `json:"errcode"`
	Errmsg      string `json:"errmsg"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type DingtalkUser struct {
	UserId     string  `json:"userid"`
	UnionId    string  `json:"unionid"`
	Name       string  `json:"name"`
	Department []int64 `json:"dept_id_list"`
	Position   string  `json:"title"`
	Mobile     string  `json:"mobile"`
	Email      string  `json:"email"`
	Avatar     string  `json:"avatar"`
	JobNumber  string  `json:"job_number"`
	Active     bool    `json:"active"`
}

type DingtalkUserListResp struct {
	Errcode   int             `json:"errcode"`
	Errmsg    string          `json:"errmsg"`
	Result    *DingtalkResult `json:"result"`
	RequestId string          `json:"request_id"`
}

type DingtalkResult struct {
	List       []*DingtalkUser `json:"list"`
	HasMore    bool            `json:"has_more"`
	NextCursor int64           `json:"next_cursor"`
}

type DingtalkDeptListResp struct {
	Errcode int    `json:"errcode"`
	Errmsg  string `json:"errmsg"`
	Result  []struct {
		DeptId int64 `json:"dept_id"`
	} `json:"result"`
	RequestId string `json:"request_id"`
}

type DingtalkDepartment struct {
	DeptId          int64  `json:"dept_id"`
	Name            string `json:"name"`
	ParentId        int64  `json:"parent_id"`
	CreateDeptGroup bool   `json:"create_dept_group"`
	AutoAddUser     bool   `json:"auto_add_user"`
}

type DingtalkDeptDetailResp struct {
	Errcode   int                 `json:"errcode"`
	Errmsg    string              `json:"errmsg"`
	Result    *DingtalkDepartment `json:"result"`
	RequestId string              `json:"request_id"`
}

// getDingtalkAccessToken gets access token from DingTalk API
func (p *DingtalkSyncerProvider) getDingtalkAccessToken() (string, error) {
	startedAt := time.Now()
	fmt.Printf("[syncer: %s/%s][DingTalk] calling gettoken\n", p.Syncer.Owner, p.Syncer.Name)

	// syncer.User should be the appKey
	// syncer.Password should be the appSecret
	appKey := p.Syncer.User
	if appKey == "" {
		return "", fmt.Errorf("appKey (user field) is required for DingTalk syncer")
	}

	appSecret := p.Syncer.Password
	if appSecret == "" {
		return "", fmt.Errorf("appSecret (password field) is required for DingTalk syncer")
	}

	apiUrl := fmt.Sprintf("https://oapi.dingtalk.com/gettoken?appkey=%s&appsecret=%s",
		url.QueryEscape(appKey), url.QueryEscape(appSecret))

	data, err := p.doDingtalkRequest("gettoken", http.MethodGet, apiUrl, nil)
	if err != nil {
		return "", err
	}

	var tokenResp DingtalkAccessTokenResp
	err = json.Unmarshal(data, &tokenResp)
	if err != nil {
		return "", err
	}

	if tokenResp.Errcode != 0 {
		return "", fmt.Errorf("failed to get access token: errcode=%d, errmsg=%s",
			tokenResp.Errcode, tokenResp.Errmsg)
	}

	fmt.Printf("[syncer: %s/%s][DingTalk] gettoken succeeded in %s, expiresIn=%ds\n", p.Syncer.Owner, p.Syncer.Name, time.Since(startedAt).Round(time.Millisecond), tokenResp.ExpiresIn)
	return tokenResp.AccessToken, nil
}

// getDingtalkDepartments gets all department IDs from DingTalk API recursively
func (p *DingtalkSyncerProvider) getDingtalkDepartments(accessToken string) ([]int64, error) {
	return p.getDingtalkDepartmentsRecursive(accessToken, 1)
}

// getDingtalkDepartmentsRecursive recursively fetches all departments starting from parentDeptId
func (p *DingtalkSyncerProvider) getDingtalkDepartmentsRecursive(accessToken string, parentDeptId int64) ([]int64, error) {
	startedAt := time.Now()
	fmt.Printf("[syncer: %s/%s][DingTalk] calling department/listsub, parentDeptId=%d\n", p.Syncer.Owner, p.Syncer.Name, parentDeptId)
	apiUrl := fmt.Sprintf("https://oapi.dingtalk.com/topapi/v2/department/listsub?access_token=%s",
		url.QueryEscape(accessToken))

	postData := map[string]interface{}{
		"dept_id": parentDeptId,
	}

	data, err := p.postJSON("department/listsub", apiUrl, postData)
	if err != nil {
		return nil, err
	}

	var deptResp DingtalkDeptListResp
	err = json.Unmarshal(data, &deptResp)
	if err != nil {
		return nil, err
	}

	if deptResp.Errcode != 0 {
		return nil, fmt.Errorf("failed to get departments: errcode=%d, errmsg=%s",
			deptResp.Errcode, deptResp.Errmsg)
	}
	fmt.Printf("[syncer: %s/%s][DingTalk] department/listsub succeeded in %s, parentDeptId=%d, children=%d\n", p.Syncer.Owner, p.Syncer.Name, time.Since(startedAt).Round(time.Millisecond), parentDeptId, len(deptResp.Result))

	// Start with the parent department itself
	deptIds := []int64{parentDeptId}

	// Recursively fetch all child departments
	for _, dept := range deptResp.Result {
		childDeptIds, err := p.getDingtalkDepartmentsRecursive(accessToken, dept.DeptId)
		if err != nil {
			return nil, err
		}
		deptIds = append(deptIds, childDeptIds...)
	}

	return deptIds, nil
}

// getDingtalkDepartmentDetails gets detailed department information
func (p *DingtalkSyncerProvider) getDingtalkDepartmentDetails(accessToken string, deptId int64) (*DingtalkDepartment, error) {
	apiUrl := fmt.Sprintf("https://oapi.dingtalk.com/topapi/v2/department/get?access_token=%s",
		url.QueryEscape(accessToken))

	postData := map[string]interface{}{
		"dept_id": deptId,
	}

	data, err := p.postJSON("department/get", apiUrl, postData)
	if err != nil {
		return nil, err
	}

	var resp DingtalkDeptDetailResp
	err = json.Unmarshal(data, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Errcode != 0 {
		return nil, fmt.Errorf("failed to get department details for %d: errcode=%d, errmsg=%s",
			deptId, resp.Errcode, resp.Errmsg)
	}

	return resp.Result, nil
}

// getDingtalkUsersFromDept gets users from a specific department
func (p *DingtalkSyncerProvider) getDingtalkUsersFromDept(accessToken string, deptId int64) ([]*DingtalkUser, error) {
	allUsers := []*DingtalkUser{}
	cursor := int64(0)
	startedAt := time.Now()

	for {
		fmt.Printf("[syncer: %s/%s][DingTalk] calling user/listsimple, deptId=%d, cursor=%d\n", p.Syncer.Owner, p.Syncer.Name, deptId, cursor)
		apiUrl := fmt.Sprintf("https://oapi.dingtalk.com/topapi/user/listsimple?access_token=%s",
			url.QueryEscape(accessToken))

		postData := map[string]interface{}{
			"dept_id": deptId,
			"cursor":  cursor,
			"size":    100,
		}

		data, err := p.postJSON("user/listsimple", apiUrl, postData)
		if err != nil {
			return nil, err
		}

		var userResp DingtalkUserListResp
		err = json.Unmarshal(data, &userResp)
		if err != nil {
			return nil, err
		}

		if userResp.Errcode != 0 {
			return nil, fmt.Errorf("failed to get users from dept %d: errcode=%d, errmsg=%s",
				deptId, userResp.Errcode, userResp.Errmsg)
		}

		if userResp.Result != nil {
			allUsers = append(allUsers, userResp.Result.List...)
			fmt.Printf("[syncer: %s/%s][DingTalk] user/listsimple page received, deptId=%d, count=%d, accumulated=%d, hasMore=%t\n", p.Syncer.Owner, p.Syncer.Name, deptId, len(userResp.Result.List), len(allUsers), userResp.Result.HasMore)

			if !userResp.Result.HasMore {
				break
			}
			cursor = userResp.Result.NextCursor
		} else {
			break
		}
	}

	fmt.Printf("[syncer: %s/%s][DingTalk] users for department completed in %s, deptId=%d, total=%d\n", p.Syncer.Owner, p.Syncer.Name, time.Since(startedAt).Round(time.Millisecond), deptId, len(allUsers))
	return allUsers, nil
}

// getDingtalkUserDetails gets detailed user information
func (p *DingtalkSyncerProvider) getDingtalkUserDetails(accessToken string, userId string) (*DingtalkUser, error) {
	apiUrl := fmt.Sprintf("https://oapi.dingtalk.com/topapi/v2/user/get?access_token=%s",
		url.QueryEscape(accessToken))

	postData := map[string]interface{}{
		"userid": userId,
	}

	data, err := p.postJSON("user/get", apiUrl, postData)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Errcode int           `json:"errcode"`
		Errmsg  string        `json:"errmsg"`
		Result  *DingtalkUser `json:"result"`
	}

	err = json.Unmarshal(data, &resp)
	if err != nil {
		return nil, err
	}

	if resp.Errcode != 0 {
		return nil, fmt.Errorf("failed to get user details for %s: errcode=%d, errmsg=%s",
			userId, resp.Errcode, resp.Errmsg)
	}

	return resp.Result, nil
}

// postJSON sends a POST request with JSON body.
func (p *DingtalkSyncerProvider) postJSON(apiName string, endpoint string, data map[string]interface{}) ([]byte, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	return p.doDingtalkRequest(apiName, http.MethodPost, endpoint, jsonData)
}

// doDingtalkRequest retries transient network, HTTP, and DingTalk throttling
// failures. Every attempt gets a fresh request body and timeout context.
func (p *DingtalkSyncerProvider) doDingtalkRequest(apiName string, method string, endpoint string, body []byte) ([]byte, error) {
	client := newSyncerHttpClient()
	var lastErr error

	for attempt := 0; attempt <= dingtalkMaxRetries; attempt++ {
		ctx, cancel := syncerHttpContext()
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, fmt.Errorf("DingTalk %s request creation failed: %w", apiName, err)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, requestErr := client.Do(req)
		if requestErr != nil {
			cancel()
			lastErr = fmt.Errorf("DingTalk %s request failed: %s", apiName, safeDingtalkNetworkError(requestErr))
			if attempt < dingtalkMaxRetries {
				p.logDingtalkRetry(apiName, attempt+1, lastErr.Error())
				time.Sleep(dingtalkRetryDelay(attempt))
				continue
			}
			return nil, lastErr
		}

		respData, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = fmt.Errorf("DingTalk %s response read failed: %s", apiName, safeDingtalkNetworkError(readErr))
			if attempt < dingtalkMaxRetries {
				p.logDingtalkRetry(apiName, attempt+1, lastErr.Error())
				time.Sleep(dingtalkRetryDelay(attempt))
				continue
			}
			return nil, lastErr
		}

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
			lastErr = fmt.Errorf("DingTalk %s returned retryable HTTP status %d", apiName, resp.StatusCode)
			if attempt < dingtalkMaxRetries {
				p.logDingtalkRetry(apiName, attempt+1, lastErr.Error())
				time.Sleep(dingtalkRetryDelay(attempt))
				continue
			}
			return nil, lastErr
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("DingTalk %s returned HTTP status %d", apiName, resp.StatusCode)
		}

		if isDingtalkThrottleResponse(respData) && attempt < dingtalkMaxRetries {
			lastErr = fmt.Errorf("DingTalk %s was throttled", apiName)
			p.logDingtalkRetry(apiName, attempt+1, lastErr.Error())
			time.Sleep(dingtalkRetryDelay(attempt))
			continue
		}

		return respData, nil
	}

	return nil, lastErr
}

func (p *DingtalkSyncerProvider) logDingtalkRetry(apiName string, retry int, reason string) {
	delay := dingtalkRetryDelay(retry - 1)
	fmt.Printf("[syncer: %s/%s][DingTalk] %s retry %d/%d in %s, reason=%s\n",
		p.Syncer.Owner, p.Syncer.Name, apiName, retry, dingtalkMaxRetries, delay, reason)
}

func dingtalkRetryDelay(retry int) time.Duration {
	return dingtalkRetryBaseDelay * time.Duration(1<<retry)
}

func safeDingtalkNetworkError(err error) string {
	if urlErr, ok := err.(*url.Error); ok && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return err.Error()
}

func isDingtalkThrottleResponse(data []byte) bool {
	var response struct {
		Errcode int             `json:"errcode"`
		SubCode json.RawMessage `json:"sub_code"`
		Subcode json.RawMessage `json:"subcode"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return false
	}

	subCode := strings.Trim(string(response.SubCode), `"`)
	if subCode == "" {
		subCode = strings.Trim(string(response.Subcode), `"`)
	}
	return response.Errcode == 88 || subCode == "90002"
}

// getDingtalkUsers gets all users from DingTalk API
func (p *DingtalkSyncerProvider) getDingtalkUsers() ([]*OriginalUser, error) {
	startedAt := time.Now()
	fmt.Printf("[syncer: %s/%s][DingTalk] user synchronization fetch started\n", p.Syncer.Owner, p.Syncer.Name)

	// Get access token
	accessToken, err := p.getDingtalkAccessToken()
	if err != nil {
		return nil, err
	}

	// Get all departments
	deptIds, err := p.getDingtalkDepartments(accessToken)
	if err != nil {
		return nil, err
	}
	fmt.Printf("[syncer: %s/%s][DingTalk] department tree fetched, departments=%d\n", p.Syncer.Owner, p.Syncer.Name, len(deptIds))

	// Get users from all departments (deduplicate by userid)
	userMap := make(map[string]*DingtalkUser)
	for _, deptId := range deptIds {
		users, err := p.getDingtalkUsersFromDept(accessToken, deptId)
		if err != nil {
			return nil, err
		}

		for _, user := range users {
			// Deduplicate users by userid
			if _, exists := userMap[user.UserId]; !exists {
				// Get detailed user information
				detailedUser, err := p.getDingtalkUserDetails(accessToken, user.UserId)
				if err != nil {
					// Use basic user info if details fail
					userMap[user.UserId] = user
				} else {
					userMap[user.UserId] = detailedUser
				}
			}
		}
	}

	// Convert DingTalk users to Casdoor OriginalUser
	originalUsers := []*OriginalUser{}
	for _, dingtalkUser := range userMap {
		originalUser := p.dingtalkUserToOriginalUser(dingtalkUser)
		originalUsers = append(originalUsers, originalUser)
	}

	fmt.Printf("[syncer: %s/%s][DingTalk] user synchronization fetch completed in %s, departments=%d, uniqueUsers=%d\n", p.Syncer.Owner, p.Syncer.Name, time.Since(startedAt).Round(time.Millisecond), len(deptIds), len(originalUsers))
	return originalUsers, nil
}

// getDingtalkUserFieldValue extracts a field value from DingtalkUser by field name
func (p *DingtalkSyncerProvider) getDingtalkUserFieldValue(dingtalkUser *DingtalkUser, fieldName string) string {
	switch fieldName {
	case "userid":
		return dingtalkUser.UserId
	case "unionid":
		return dingtalkUser.UnionId
	case "name":
		return dingtalkUser.Name
	case "email":
		return dingtalkUser.Email
	case "mobile":
		return dingtalkUser.Mobile
	case "avatar":
		return dingtalkUser.Avatar
	case "title":
		return dingtalkUser.Position
	case "job_number":
		return dingtalkUser.JobNumber
	case "active":
		// Invert the boolean because active=true means NOT forbidden
		return util.BoolToString(!dingtalkUser.Active)
	default:
		return ""
	}
}

// dingtalkUserToOriginalUser converts DingTalk user to Casdoor OriginalUser
func (p *DingtalkSyncerProvider) dingtalkUserToOriginalUser(dingtalkUser *DingtalkUser) *OriginalUser {
	user := &OriginalUser{
		Address:    []string{},
		Properties: map[string]string{},
		Groups:     []string{},
		DingTalk:   dingtalkUser.UserId, // Link DingTalk provider account
	}

	// Apply TableColumns mapping if configured
	if len(p.Syncer.TableColumns) > 0 {
		for _, tableColumn := range p.Syncer.TableColumns {
			value := p.getDingtalkUserFieldValue(dingtalkUser, tableColumn.Name)
			p.Syncer.setUserByKeyValue(user, tableColumn.CasdoorName, value)
		}
	} else {
		// Fallback to default mapping for backward compatibility
		user.Id = dingtalkUser.UserId
		user.Name = dingtalkUser.UserId
		if dingtalkUser.UnionId != "" {
			user.Name = dingtalkUser.UnionId
		}
		user.DisplayName = dingtalkUser.Name
		user.Email = dingtalkUser.Email
		user.Phone = dingtalkUser.Mobile
		user.Avatar = dingtalkUser.Avatar
		user.Title = dingtalkUser.Position
		user.IsForbidden = !dingtalkUser.Active
	}

	// Add department IDs to Groups field
	for _, deptId := range dingtalkUser.Department {
		user.Groups = append(user.Groups, p.getDingtalkGroupId(deptId))
	}

	// Set CreatedTime to current time if not set
	if user.CreatedTime == "" {
		user.CreatedTime = util.GetCurrentTime()
	}

	return user
}

// GetOriginalGroups retrieves all groups (departments) from DingTalk
func (p *DingtalkSyncerProvider) GetOriginalGroups() ([]*OriginalGroup, error) {
	startedAt := time.Now()
	fmt.Printf("[syncer: %s/%s][DingTalk] group synchronization fetch started\n", p.Syncer.Owner, p.Syncer.Name)

	// Get access token
	accessToken, err := p.getDingtalkAccessToken()
	if err != nil {
		return nil, err
	}

	// Get all department IDs
	deptIds, err := p.getDingtalkDepartments(accessToken)
	if err != nil {
		return nil, err
	}

	// Get detailed information for each department
	originalGroups := []*OriginalGroup{}
	for _, deptId := range deptIds {
		dept, err := p.getDingtalkDepartmentDetails(accessToken, deptId)
		if err != nil {
			// Log error but continue with other departments
			fmt.Printf("Warning: failed to get details for department %d: %v\n", deptId, err)
			continue
		}

		originalGroup := p.dingtalkDepartmentToOriginalGroup(dept)
		originalGroups = append(originalGroups, originalGroup)
	}

	fmt.Printf("[syncer: %s/%s][DingTalk] group synchronization fetch completed in %s, departments=%d, groups=%d\n", p.Syncer.Owner, p.Syncer.Name, time.Since(startedAt).Round(time.Millisecond), len(deptIds), len(originalGroups))
	return originalGroups, nil
}

// getDingtalkGroupName returns the Casdoor group name of a DingTalk department. The
// department ID is used as the name because DingTalk department names are not unique.
func getDingtalkGroupName(deptId int64) string {
	return fmt.Sprintf("%d", deptId)
}

// getDingtalkGroupId returns the Casdoor group ID ("organization/name") of a DingTalk
// department. User.Groups holds full group IDs, so a bare department ID would not match
// any group and the membership would be silently dropped by the group and permission APIs.
func (p *DingtalkSyncerProvider) getDingtalkGroupId(deptId int64) string {
	return util.GetId(p.Syncer.Organization, getDingtalkGroupName(deptId))
}

// dingtalkDepartmentToOriginalGroup converts DingTalk department to Casdoor OriginalGroup
func (p *DingtalkSyncerProvider) dingtalkDepartmentToOriginalGroup(dept *DingtalkDepartment) *OriginalGroup {
	deptIdStr := getDingtalkGroupName(dept.DeptId)

	return &OriginalGroup{
		Id:          p.getDingtalkGroupId(dept.DeptId),
		Name:        deptIdStr,    // Use ID as name for uniqueness
		DisplayName: dept.Name,    // Use actual name as display name
		Description: "",           // DingTalk doesn't provide description
		Type:        "department", // Mark as department type
		Manager:     "",           // DingTalk doesn't provide manager in dept details
		Email:       "",           // DingTalk doesn't provide email for departments
	}
}

// GetOriginalUserGroups retrieves the group (department) IDs that a user belongs to
func (p *DingtalkSyncerProvider) GetOriginalUserGroups(userId string) ([]string, error) {
	// Get access token
	accessToken, err := p.getDingtalkAccessToken()
	if err != nil {
		return nil, err
	}

	// Get detailed user information which includes department list
	user, err := p.getDingtalkUserDetails(accessToken, userId)
	if err != nil {
		return nil, err
	}

	// Convert department IDs to Casdoor group IDs
	groupIds := []string{}
	for _, deptId := range user.Department {
		groupIds = append(groupIds, p.getDingtalkGroupId(deptId))
	}

	return groupIds, nil
}
