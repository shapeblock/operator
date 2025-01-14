package utils

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type GitClient struct {
	apiURL string
	apiKey string
	client *http.Client
}

func NewGitClient(apiURL, apiKey string) *GitClient {
	return &GitClient{
		apiURL: apiURL,
		apiKey: apiKey,
		client: &http.Client{},
	}
}

func (g *GitClient) GetSSHKey(appName string) (string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/apps/%s/ssh-key", g.apiURL, appName), nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", g.apiKey))

	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get SSH key: %s", resp.Status)
	}

	var result struct {
		PrivateKey string `json:"private_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result.PrivateKey, nil
}
