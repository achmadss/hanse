package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/version"
)

const (
	// agentProtocolV1 is the protocol version the agent speaks
	// (docs/agent-protocol.md §12).
	agentProtocolV1 = "hanse.agent/v1"
	// credentialFileName is the credential's file name inside the state
	// directory (docs/configuration.md §8.2).
	credentialFileName = "credential"
	// credentialFileVersion is the stored file's format version.
	credentialFileVersion = 1
	// joinTimeout bounds the whole join exchange.
	joinTimeout = 30 * time.Second
	// maxJoinResponse is the largest join response the agent reads. A
	// successful one holds two short strings.
	maxJoinResponse = 64 << 10
)

// joinInput is the POST /v1/agent/join body (docs/agent-protocol.md §2.2).
type joinInput struct {
	Protocol     string `json:"protocol"`
	JoinToken    string `json:"join_token"`
	AgentVersion string `json:"agent_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
}

// joinOutput is the successful join response (docs/agent-protocol.md §2.3).
type joinOutput struct {
	Node       string `json:"node"`
	Credential string `json:"credential"`
}

// credentialFile is the stored credential file. It keeps the node ID and
// the Gateway URL beside the credential, so the agent needs no other source
// for them when it connects (issue #38).
type credentialFile struct {
	Version    int    `json:"version"`
	GatewayURL string `json:"gateway_url"`
	Node       string `json:"node"`
	Credential string `json:"credential"`
}

// runJoin exchanges a join token for a node credential and stores it in
// the state directory. The credential and the token never appear in
// output: the printed line names the node and the file only.
func runJoin(args []string, stdout, stderr io.Writer, stateDir string) int {
	positionals, dir, configPath, ok := parseJoinArgs(args, stateDir, stderr)
	if !ok {
		return exitUse
	}
	if len(positionals) != 2 {
		joinUsage(stderr)
		return exitUse
	}
	gatewayURL, joinToken := positionals[0], positionals[1]
	endpoint, err := joinEndpoint(gatewayURL)
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	out, err := exchangeJoin(endpoint, joinToken)
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	path, err := writeCredential(dir, credentialFile{
		Version:    credentialFileVersion,
		GatewayURL: gatewayURL,
		Node:       out.Node,
		Credential: out.Credential,
	})
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	if configPath != "" {
		if err := writeGatewayURL(configPath, gatewayURL); err != nil {
			printf(stderr, "hanse-node: %v\n", err)
			return exitFail
		}
	}
	printf(stdout, "enrolled node %s; the credential is stored in %s\n", out.Node, path)
	return exitOK
}

func joinUsage(w io.Writer) {
	printf(w, "usage: hanse-node join <gateway-url> <join-token> [--state-dir <path>] [--config <path>]\n")
}

// parseJoinArgs scans the join arguments. The Gateway URL and the join
// token are positionals, and --state-dir and --config may appear anywhere,
// so `hanse-node join --state-dir X <url> <token>` and
// `hanse-node join <url> <token> --state-dir X` name the same directory.
// configPath is empty unless --config was given: only then does join write
// gateway_url into agent.yaml (docs/configuration.md section 7.1).
func parseJoinArgs(args []string, stateDir string, stderr io.Writer) (positionals []string, dir, configPath string, ok bool) {
	dir = stateDir
	for i := 0; i < len(args); i++ {
		s := args[i]
		name := strings.TrimLeft(s, "-")
		value, hasEq := "", false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value, hasEq = name[:eq], name[eq+1:], true
		}
		switch name {
		case "state-dir", "config":
			if !hasEq {
				if i+1 >= len(args) {
					printf(stderr, "hanse-node: %s needs a value\n", s)
					return nil, "", "", false
				}
				i++
				value = args[i]
			}
			if value == "" {
				printf(stderr, "hanse-node: %s needs a value\n", s)
				return nil, "", "", false
			}
			if name == "state-dir" {
				dir = value
			} else {
				configPath = value
			}
		default:
			if strings.HasPrefix(s, "-") {
				printf(stderr, "hanse-node: unknown flag: %s\n", s)
				return nil, "", "", false
			}
			positionals = append(positionals, s)
		}
	}
	return positionals, dir, configPath, true
}

// joinEndpoint validates the Gateway URL and returns the join endpoint.
// The agent must use https, and may use http only when the host is a
// loopback address (docs/agent-protocol.md §1).
func joinEndpoint(gatewayURL string) (string, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("the Gateway URL is not a valid URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && config.IsLoopbackHost(u.Hostname()):
	default:
		return "", errors.New("the Gateway URL must use https, unless its host is a loopback address")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("the Gateway URL must not carry a query, a fragment, or a user name")
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/agent/join"
	return u.String(), nil
}

// exchangeJoin posts the join request and returns the Gateway's answer. A
// refusal names the Gateway's error code; it never names the join token.
func exchangeJoin(endpoint, joinToken string) (joinOutput, error) {
	body, err := json.Marshal(joinInput{
		Protocol:     agentProtocolV1,
		JoinToken:    joinToken,
		AgentVersion: version.Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
	})
	if err != nil {
		return joinOutput{}, fmt.Errorf("cannot build the join request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return joinOutput{}, fmt.Errorf("cannot build the join request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: joinTimeout,
		// A redirect would resend the body, which carries the join token,
		// to another location that may be plain http or another host. The
		// join endpoint never redirects, so refuse every redirect and keep
		// the answer as it is.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return joinOutput{}, fmt.Errorf("cannot reach the Gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJoinResponse))
	if err != nil {
		return joinOutput{}, fmt.Errorf("cannot read the Gateway's answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return joinOutput{}, gatewayError(resp.StatusCode, data)
	}
	var out joinOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return joinOutput{}, errors.New("the Gateway's answer is not a join response")
	}
	if out.Node == "" || !strings.HasPrefix(out.Credential, "hnc_") {
		return joinOutput{}, errors.New("the Gateway's answer is missing the node or the credential")
	}
	return out, nil
}

// gatewayError turns a non-200 join answer into an error. It keeps the
// Gateway's code and message, which carry no secret, and reads the body as
// text only.
func gatewayError(status int, body []byte) error {
	var wire struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &wire); err == nil && wire.Code != "" {
		return fmt.Errorf("the Gateway refused the join: %s (%s)", wire.Message, wire.Code)
	}
	return fmt.Errorf("the Gateway refused the join with HTTP %d", status)
}

// writeCredential stores the node credential in <state_dir>/credential and
// returns the path. The directory is created with mode 0700 and the file
// with mode 0600, so only the agent's account can read it
// (docs/configuration.md §8.2). The value goes through a temporary file in
// the same directory, so a crash never leaves a half-written credential.
//
// On Windows POSIX mode bits do not exist, so Go cannot narrow the file
// further: the file inherits the state directory's ACL. The default
// directory under C:\ProgramData grants access to Administrators and SYSTEM
// only, and the agent account is the service's own account.
func writeCredential(stateDir string, body credentialFile) (string, error) {
	if stateDir == "" {
		return "", errors.New("the state directory is empty")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create the state directory %s: %v", stateDir, err)
	}
	if err := restrictMode(stateDir, 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return "", fmt.Errorf("cannot encode the credential: %v", err)
	}
	data = append(data, '\n')
	path := filepath.Join(stateDir, credentialFileName)
	tmp, err := os.CreateTemp(stateDir, ".credential-*")
	if err != nil {
		return "", fmt.Errorf("cannot create the credential file: %v", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("cannot write the credential file: %v", err)
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return "", fmt.Errorf("cannot set the mode of the credential file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cannot write the credential file: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("cannot store the credential file: %v", err)
	}
	return path, nil
}

// restrictMode tightens the mode of path on Unix. Windows has no POSIX mode
// bits; its ACL rule is future work, as in internal/enroll and internal/auth.
func restrictMode(path string, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("cannot set the mode of %s: %v", path, err)
	}
	return nil
}

// writeGatewayURL stores gateway_url in agent.yaml, keeping every other
// value and comment, so a fresh enrollment leaves the node ready to run
// (docs/configuration.md section 7.1). A missing file is created with the
// format version and the URL.
func writeGatewayURL(path, gatewayURL string) error {
	if path == "" {
		return nil
	}
	var doc yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(data)) > 0 {
			if err := yaml.Unmarshal(data, &doc); err != nil {
				return fmt.Errorf("cannot read %s: %v", path, err)
			}
		}
	case os.IsNotExist(err):
	default:
		return fmt.Errorf("cannot read %s: %v", path, err)
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a YAML mapping", path)
	}
	mapping := doc.Content[0]
	value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: gatewayURL}
	found := false
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "gateway_url" {
			mapping.Content[i+1] = value
			found = true
			break
		}
	}
	if !found {
		if len(mapping.Content) == 0 {
			mapping.Content = append(mapping.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "version"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"},
			)
		}
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "gateway_url"},
			value,
		)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("cannot encode %s: %v", path, err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %v", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".agent-*")
	if err != nil {
		return fmt.Errorf("cannot write %s: %v", path, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cannot write %s: %v", path, err)
	}
	if err := tmp.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("cannot set the mode of %s: %v", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot write %s: %v", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("cannot store %s: %v", path, err)
	}
	return nil
}
