// Package mano provides a client for the MANO CNF lifecycle management API.
//
// It is used to toggle spec.replication.channels[0].isSource on a PXC CNF
// via the operator's own API rather than calling the k8s API directly.
//
// Flow:
//  1. POST /users/auth                                              → Bearer token
//  2. POST /cnflcm/v1/custom-resources/{cnf}/{vdu}/update  → 202, lcmOpOccId in header
//  3. Poll GET /cnflcm/v1/lcm-op-occ/{id}                 → wait for operationState=COMPLETED
package mano

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Client is an HTTP client for the MANO CNF LCM API.
// It supports two authentication modes:
//   - Static token: pass a pre-obtained Bearer token via NewClient.
//   - Credentials: pass username/password via NewClientWithCredentials; the client
//     auto-logins via POST /users/auth and refreshes the token before it expires.
type Client struct {
	baseURL    string
	httpClient *http.Client

	// static token mode
	staticToken string

	// credentials mode
	username string
	password string

	// cached token state (credentials mode only)
	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

// newHTTPClient builds an http.Client. When tlsInsecure is true the TLS
// certificate verification is disabled — use only when the MANO server is
// addressed by IP and its certificate carries no IP SANs.
func newHTTPClient(tlsInsecure bool) *http.Client {
	if !tlsInsecure {
		return &http.Client{Timeout: 30 * time.Second}
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // operator opt-in via tlsInsecureSkipVerify
		},
	}
}

// NewClient creates a MANO API client using a pre-obtained static Bearer token.
// The token is used as-is for every request without refresh.
func NewClient(baseURL, token string, tlsInsecure bool) *Client {
	return &Client{
		baseURL:     baseURL,
		staticToken: token,
		httpClient:  newHTTPClient(tlsInsecure),
	}
}

// NewClientWithCredentials creates a MANO API client that auto-obtains and
// refreshes Bearer tokens via POST /users/auth using Basic auth.
// username and password are the MANO user credentials.
func NewClientWithCredentials(baseURL, username, password string, tlsInsecure bool) *Client {
	return &Client{
		baseURL:    baseURL,
		username:   username,
		password:   password,
		httpClient: newHTTPClient(tlsInsecure),
	}
}

// ensureToken returns a valid Bearer token, logging in if the cached token is
// missing or about to expire. For static token clients it returns the static value.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	if c.staticToken != "" {
		return c.staticToken, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Refresh 60 seconds before the real expiry to avoid race at boundary.
	if c.cachedToken != "" && time.Now().Add(60*time.Second).Before(c.tokenExpiry) {
		return c.cachedToken, nil
	}

	token, expiry, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.cachedToken = token
	c.tokenExpiry = expiry
	return token, nil
}

// login calls POST /users/auth with Basic auth and returns the access token and expiry.
func (c *Client) login(ctx context.Context) (token string, expiry time.Time, err error) {
	body, err := json.Marshal(map[string]string{"grant_type": "client_credentials"})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal login request: %w", err)
	}

	url := c.baseURL + "/users/auth"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("build login request: %w", err)
	}

	creds := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
	req.Header.Set("Authorization", "Basic "+creds)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("MANO login returned HTTP %d (expected 200)", resp.StatusCode)
	}

	var result accessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", time.Time{}, fmt.Errorf("decode login response: %w", err)
	}
	if result.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("MANO login response missing access_token")
	}

	// expires_in is seconds as a string per the MANO API spec.
	var expiresIn int64 = 28800 // default 8 hours
	if result.ExpiresIn != "" {
		if n, parseErr := strconv.ParseInt(result.ExpiresIn, 10, 64); parseErr == nil {
			expiresIn = n
		}
	}

	return result.AccessToken, time.Now().Add(time.Duration(expiresIn) * time.Second), nil
}

// SetIsSource calls the MANO API to set replicationChannels[0].isSource on the given CNF/VDU,
// then polls until the LCM operation reaches COMPLETED or fails.
func (c *Client) SetIsSource(ctx context.Context, cnfName, vduName string, isSource bool, pollInterval, pollTimeout time.Duration) error {
	token, err := c.ensureToken(ctx)
	if err != nil {
		return fmt.Errorf("obtain MANO token: %w", err)
	}

	isSourceStr := "false"
	if isSource {
		isSourceStr = "true"
	}

	reqBody := updateRequest{
		CNFName: cnfName,
		VDUName: vduName,
		UpdateVduOperatorRequest: updateVduOperatorRequest{
			Description:  fmt.Sprintf("mysql-keeper: set isSource=%s", isSourceStr),
			TimeoutTimer: fmt.Sprintf("%ds", int(pollTimeout.Seconds())),
			UpdateFields: map[string]string{
				"replicationChannels[0].isSource": isSourceStr,
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal MANO request: %w", err)
	}

	url := fmt.Sprintf("%s/cnflcm/v1/custom-resources/cnf/vdu/update", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("build MANO update request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("MANO update returned HTTP %d (expected 202)", resp.StatusCode)
	}

	lcmOpOccID := resp.Header.Get("lcmOpOccId")
	if lcmOpOccID == "" {
		return fmt.Errorf("MANO 202 response missing lcmOpOccId header")
	}

	return c.pollLcmOpOcc(ctx, lcmOpOccID, token, pollInterval, pollTimeout)
}

// pollLcmOpOcc polls GET /cnflcm/v1/lcm-op-occ/{id} until operationState is terminal.
func (c *Client) pollLcmOpOcc(ctx context.Context, id, token string, pollInterval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("%s/cnflcm/v1/lcm-op-occ/%s", c.baseURL, id)

	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("MANO LCM op %s did not complete within %s", id, timeout)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build poll request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("GET %s: %w", url, err)
		}

		var occ lcmOpOcc
		decodeErr := json.NewDecoder(resp.Body).Decode(&occ)
		resp.Body.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode lcm-op-occ response: %w", decodeErr)
		}

		switch occ.OperationState {
		case "COMPLETED":
			return nil
		case "FAILED", "FAILED_TEMP", "ROLLED_BACK":
			detail := occ.OperationState
			if occ.Error != nil {
				detail = occ.Error.Detail
			}
			return fmt.Errorf("MANO LCM op %s failed: %s", id, detail)
		}
		// STARTING / PROCESSING — wait and retry.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// --- request / response types ---

type updateRequest struct {
	CNFName                  string                   `json:"cnfName"`
	VDUName                  string                   `json:"vduName"`
	UpdateVduOperatorRequest updateVduOperatorRequest `json:"updateVduOperatorRequest"`
}

type updateVduOperatorRequest struct {
	Description  string            `json:"description"`
	TimeoutTimer string            `json:"timeoutTimer"`
	UpdateFields map[string]string `json:"updateFields"`
}

// LcmOpOcc mirrors the VnfLcmOpOcc response type from the MANO API.
type lcmOpOcc struct {
	ID             string          `json:"id"`
	OperationState string          `json:"operationState"`
	Error          *problemDetails `json:"error"`
}

type problemDetails struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// accessTokenResponse mirrors POST /users/auth response body.
type accessTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   string `json:"expires_in"` // seconds as string per MANO API spec
	UserID      string `json:"userId"`
}
