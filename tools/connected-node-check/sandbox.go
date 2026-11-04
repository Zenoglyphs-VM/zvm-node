package main

import (
	"io"
	"net"
	"os"
	"strings"
)

// Verify the actual container boundary without printing environment values or
// host mount paths. Only public chain inputs are made available to the client.
func verifySandbox() {
	check(os.Getuid() > 0 && os.Getgid() > 0, "qualification must use its non-root data owner")
	entries, err := os.ReadDir("/source")
	must(err)
	check(len(entries) == 1 && entries[0].Name() == "chains" && entries[0].IsDir(), "container source input scope differs")
	_, err = os.Stat("/source/.git")
	check(os.IsNotExist(err), "Git metadata is accessible inside the client container")
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN", "ACTIONS_RUNTIME_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"} {
		_, exists := os.LookupEnv(name)
		check(!exists, "GitHub credential was provided to the client container")
	}
	interfaces, err := net.Interfaces()
	must(err)
	check(len(interfaces) == 1 && interfaces[0].Flags&net.FlagLoopback != 0, "container has a non-loopback interface")
	f, err := os.Open("/proc/self/mountinfo")
	must(err)
	raw, err := io.ReadAll(io.LimitReader(f, 128<<10+1))
	must(err)
	must(f.Close())
	check(len(raw) <= 128<<10, "mount metadata cap exceeded")
	rootReadOnly, inputsReadOnly := false, false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		for _, option := range strings.Split(fields[5], ",") {
			if option == "ro" && fields[4] == "/" {
				rootReadOnly = true
			}
			if option == "ro" && fields[4] == "/source/chains" {
				inputsReadOnly = true
			}
		}
	}
	check(rootReadOnly && inputsReadOnly, "container root or chain inputs are writable")
	for key, path := range map[string]string{"XDG_DATA_HOME": "/data/state", "XDG_CONFIG_HOME": "/data/config", "XDG_CACHE_HOME": "/data/cache"} {
		check(os.Getenv(key) == path, "client data directory is outside owned data")
		must(os.Mkdir(path, 0700))
	}
	output("SANDBOX", map[string]any{"nonRootOwner": true, "loopbackOnly": true, "readOnlyRoot": true,
		"readOnlyPublicChainInputsOnly": true, "gitMetadataAbsent": true, "GitHubCredentialVariablesAbsent": true, "dataDirectoriesOwned": true})
}
