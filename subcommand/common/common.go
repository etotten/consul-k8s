// Package common holds code needed by multiple commands.
package common

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
)

const (
	// ACLReplicationTokenName is the name used for the ACL replication policy and
	// Kubernetes secret. It is consumed in both the server-acl-init and
	// create-federation-secret commands and so lives in this common package.
	ACLReplicationTokenName = "acl-replication"

	// ACLTokenSecretKey is the key that we store the ACL tokens in when we
	// create Kubernetes secrets.
	ACLTokenSecretKey = "token"
)

// Logger returns an hclog instance or an error if level is invalid.
func Logger(level string) (hclog.Logger, error) {
	parsedLevel := hclog.LevelFromString(level)
	if parsedLevel == hclog.NoLevel {
		return nil, fmt.Errorf("unknown log level: %s", level)
	}
	return hclog.New(&hclog.LoggerOptions{
		Level:  parsedLevel,
		Output: os.Stderr,
	}), nil
}

// ValidatePort converts flags representing ports into integer and validates
// that it's in the port range.
func ValidatePort(flagName, flagValue string) error {
	port, err := strconv.Atoi(flagValue)
	if err != nil {
		return errors.New(fmt.Sprintf("%s value of %s is not a valid integer.", flagName, flagValue))
	}
	// This checks if the port is in the valid port range.
	if port < 1024 || port > 65535 {
		return errors.New(fmt.Sprintf("%s value of %d is not in the port range 1024-65535.", flagName, port))
	}
	return nil
}

// ConsulLogin issues an ACL().Login to Consul and writes out the token to tokenSinkFile.
func ConsulLogin(client *api.Client, bearerTokenFile, authMethodName, tokenSinkFile string, meta map[string]string) error {
	data, err := ioutil.ReadFile(bearerTokenFile)
	if err != nil {
		return fmt.Errorf("unable to read bearerTokenFile: %v, err: %v", bearerTokenFile, err)
	}
	bearerToken := strings.TrimSpace(string(data))
	if bearerToken == "" {
		return fmt.Errorf("no bearer token found in %s", bearerTokenFile)
	}
	// Do the login.
	req := &api.ACLLoginParams{
		AuthMethod:  authMethodName, // "consul-k8s-auth-method"
		BearerToken: bearerToken,    // /var/run/secrets/kubernetes.io/serviceaccount/token
		Meta:        meta,           // pod=default/podName
	}
	tok, _, err := client.ACL().Login(req, nil)
	if err != nil {
		return fmt.Errorf("error logging in: %s", err)
	}

	// Write the token out to file.
	payload := []byte(tok.SecretID)
	if err := ioutil.WriteFile(tokenSinkFile, payload, 444); err != nil {
		return fmt.Errorf("error writing token to file sink: %v", err)
	}
	return nil
}

// WriteTempFile writes contents to a temporary file and returns the file
// name. It will remove the file once the test completes.
func WriteTempFile(t *testing.T, contents string) string {
	t.Helper()
	file, err := ioutil.TempFile("", "")
	require.NoError(t, err)
	_, err = file.WriteString(contents)
	require.NoError(t, err)

	t.Cleanup(func() {
		os.Remove(file.Name())
	})

	return file.Name()
}
