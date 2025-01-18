package license

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"
)

const (
	keygenAccountID = "53666519-ebe7-4ca2-9c1a-d026831e4b56"
	keygenBaseURL   = "https://api.keygen.sh/v1/accounts/" + keygenAccountID
	licensePattern  = `^[A-F0-9]{6}-[A-F0-9]{6}-[A-F0-9]{6}-[A-F0-9]{6}-[A-F0-9]{6}-V3$`
)

type Validator struct {
	client      *http.Client
	licenseKey  string
	email       string
	fingerprint string
}

func NewValidator(licenseKey, email string) (*Validator, error) {
	if matched, _ := regexp.MatchString(licensePattern, licenseKey); !matched {
		return nil, fmt.Errorf("invalid license format")
	}

	// Create fingerprint from email
	fingerprint := sha256.Sum256([]byte(email))
	fingerprintHex := hex.EncodeToString(fingerprint[:])

	return &Validator{
		client:      &http.Client{Timeout: 10 * time.Second},
		licenseKey:  licenseKey,
		email:       email,
		fingerprint: fingerprintHex,
	}, nil
}

func (v *Validator) ValidateAndActivate() error {
	// First validate the license
	validateURL := fmt.Sprintf("%s/licenses/actions/validate-key", keygenBaseURL)
	validatePayload := map[string]interface{}{
		"meta": map[string]interface{}{
			"key": v.licenseKey,
			"scope": map[string]interface{}{
				"fingerprint": v.fingerprint,
			},
		},
	}

	resp, err := v.makeRequest("POST", validateURL, validatePayload)
	if err != nil {
		return fmt.Errorf("license validation failed: %v", err)
	}

	meta := resp["meta"].(map[string]interface{})
	if meta["valid"].(bool) {
		return nil
	}

	code, ok := meta["code"].(string)
	if !ok {
		return fmt.Errorf("unexpected response format")
	}

	if code != "NO_MACHINE" {
		return fmt.Errorf("invalid license: %s", meta["detail"].(string))
	}

	// Get license ID
	licenseID, err := v.getLicenseID()
	if err != nil {
		return err
	}

	// Activate the license
	return v.activateLicense(licenseID)
}

func (v *Validator) getLicenseID() (string, error) {
	meURL := fmt.Sprintf("%s/me", keygenBaseURL)
	resp, err := v.makeRequest("GET", meURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get license info: %v", err)
	}

	licenseData := resp["data"].(map[string]interface{})
	licenseID := licenseData["id"].(string)
	status := licenseData["attributes"].(map[string]interface{})["status"].(string)

	if status != "ACTIVE" {
		return "", fmt.Errorf("license is not active (status: %s)", status)
	}

	return licenseID, nil
}

func (v *Validator) activateLicense(licenseID string) error {
	activateURL := fmt.Sprintf("%s/machines", keygenBaseURL)
	activatePayload := map[string]interface{}{
		"data": map[string]interface{}{
			"type": "machines",
			"attributes": map[string]interface{}{
				"fingerprint": v.fingerprint,
			},
			"relationships": map[string]interface{}{
				"license": map[string]interface{}{
					"data": map[string]interface{}{
						"type": "licenses",
						"id":   licenseID,
					},
				},
			},
		},
	}

	_, err := v.makeRequest("POST", activateURL, activatePayload)
	return err
}

func (v *Validator) makeRequest(method, url string, payload interface{}) (map[string]interface{}, error) {
	var req *http.Request
	var err error
	var reqBody io.Reader

	if payload != nil {
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(payloadBytes)
	}

	req, err = http.NewRequest(method, url, reqBody)

	if err != nil {
		return nil, err
	}

	// Let's add some debug logging
	fmt.Printf("Request URL: %s\n", url)
	fmt.Printf("Request Headers: %+v\n", req.Header)
	fmt.Printf("Request Payload: %+v\n", payload)

	req.Header.Set("Content-Type", "application/vnd.api+json")
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("Authorization", "License "+v.licenseKey)

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Add response debug logging
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Response Status: %d\n", resp.StatusCode)
	fmt.Printf("Response Body: %s\n", string(body))

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("request failed with status %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result, nil
}
