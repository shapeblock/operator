package utils

import "strings"

// ParseImageString splits a Docker image string into repository and tag
func ParseImageString(image string) (string, string) {
	parts := strings.Split(image, ":")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	// If no tag is specified, return "latest" as default
	return image, "latest"
}
