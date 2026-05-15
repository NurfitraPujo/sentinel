package testcontainers

import (
	"os"

	"github.com/testcontainers/testcontainers-go"
)

func DetectProvider() testcontainers.ProviderType {
	provider := os.Getenv("TESTCONTAINERS_PROVIDER")
	switch provider {
	case "podman":
		return testcontainers.ProviderPodman
	default:
		return testcontainers.ProviderDocker
	}
}

func ConfigureProvider() testcontainers.ProviderType {
	if DetectProvider() == testcontainers.ProviderPodman {
		os.Setenv("TESTCONTAINERS_PODMAN", "true")
		return testcontainers.ProviderPodman
	} else {
		os.Unsetenv("TESTCONTAINERS_PODMAN")
		return testcontainers.ProviderDocker
	}
}
