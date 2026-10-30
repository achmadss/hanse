package registry

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// manifestErr builds an error that names the manifest file and the
// manifest field the problem is in, per issue #10's "path and field".
func manifestErr(manifestPath, field, format string, args ...any) error {
	return herrors.New(herrors.Internal,
		fmt.Sprintf("%s: %s: %s", errPath(manifestPath), field, fmt.Sprintf(format, args...)), nil)
}

// opField renders the manifest field of one operation, such as
// "operations.up.concurrency".
func opField(opID, field string) string {
	var b strings.Builder
	b.WriteString("operations.")
	b.WriteString(opID)
	if field != "" {
		b.WriteString(".")
		b.WriteString(field)
	}
	return b.String()
}

// hasOperations reports whether the manifest had an operations key. An
// absent key decodes to a nil map.

// OperationIDs returns the manifest's operation IDs sorted.
func (m *Manifest) OperationIDs() []string {
	ids := make([]string, 0, len(m.Operations))
	for id := range m.Operations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// validateManifest applies every manifest rule from docs/configuration.md
// sections 6 and 10 that does not need the compiled schema; the
// schema-dependent checks happen in buildOperation. It rejects
// duplicate operation keys before this runs: yaml.v3 already reports a
// duplicate mapping key as a decode error.
func validateManifest(path string, m *Manifest) error {
	if m.Version != 1 {
		return manifestErr(path, "version", "version is required and must be 1")
	}
	if !config.ValidID(m.ID) {
		return manifestErr(path, "id", "id must match ^[a-z][a-z0-9_-]*$ and be at most 63 characters long, not %q", m.ID)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if strings.TrimSpace(m.Description) == "" {
		return manifestErr(path, "description", "description is required")
	}
	if m.Contract != ContractV1 {
		return manifestErr(path, "contract", "contract must be %q, got %q", ContractV1, m.Contract)
	}
	if err := validateHandler(path, m); err != nil {
		return err
	}
	ids := m.OperationIDs()
	for _, id := range ids {
		if !config.ValidID(id) {
			return manifestErr(path, opField(id, ""), "operation ID must match ^[a-z][a-z0-9_-]*$ and be at most 63 characters long, got %q", id)
		}
		if err := validateOperation(path, m, id, m.Operations[id]); err != nil {
			return err
		}
	}
	return nil
}

// validateHandler validates the manifest's handler block (section 6.1).
func validateHandler(path string, m *Manifest) error {
	h := &m.Handler
	switch h.Kind {
	case KindExec:
		if len(h.Entry) > 0 {
			for osname, cmd := range h.Entry {
				if err := entryCommand(osname, cmd); err != nil {
					return manifestErr(path, "handler.entry."+osname, "%s", err)
				}
			}
		}
		if len(h.Entry) == 0 {
			return manifestErr(path, "handler.entry", "the entry command is required for at least one of linux, darwin, or windows")
		}
		if h.URL != "" {
			return manifestErr(path, "handler.url", "url is http only")
		}
		if h.Auth != nil {
			return manifestErr(path, "handler.auth", "auth is http only")
		}
		if h.CAFile != "" {
			return manifestErr(path, "handler.ca_file", "ca_file is http only")
		}
	case KindHTTP:
		for osname := range h.Entry {
			return manifestErr(path, "handler.entry."+osname, "entry is exec only")
		}
		if h.URL == "" {
			return manifestErr(path, "handler.url", "url is required for http handlers")
		}
		if err := validateURL(h.URL); err != nil {
			return manifestErr(path, "handler.url", "%s", err)
		}
		if err := validateAuth(path, h.Auth); err != nil {
			return err
		}
	default:
		return manifestErr(path, "handler.kind", "handler.kind is required and must be %q or %q, got %q", KindExec, KindHTTP, h.Kind)
	}
	return validateSecrets(path, m)
}

// entryCommand validates one entry.<os> value: a known OS and a
// non-empty command argument list.
func entryCommand(osname string, cmd []string) error {
	if !pureOS(osname) {
		return fmt.Errorf("entry OS must be linux, darwin, or windows, got %q", osname)
	}
	if len(cmd) == 0 {
		return fmt.Errorf("the entry command needs at least one argument")
	}
	for i, arg := range cmd {
		if arg == "" {
			return fmt.Errorf("entry command argument %d is empty", i)
		}
	}
	return nil
}

// validateAuth validates handler.auth (section 6.1): type, header name,
// and the credential reference.
func validateAuth(path string, auth *HandlerAuth) error {
	if auth == nil {
		return nil
	}
	switch auth.Type {
	case "bearer":
		if auth.Header != "" {
			return manifestErr(path, "handler.auth.header", "auth.header is used only with auth.type header")
		}
	case "header":
		if auth.Header == "" {
			return manifestErr(path, "handler.auth.header", "auth.header is required when auth.type is header")
		}
	default:
		return manifestErr(path, "handler.auth.type", "auth.type must be %q or %q, got %q", "bearer", "header", auth.Type)
	}
	if err := nameReference(auth.Credential); err != nil {
		return manifestErr(path, "handler.auth.credential", "%s", err)
	}
	return nil
}

// nameReference rejects an empty name and one that escapes its containing
// directory. Secrets and credentials resolve to files under
// credentials_dir, section 8.1.
func nameReference(name string) error {
	if name == "" {
		return fmt.Errorf("the name is required")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) || strings.Contains(name, ":") {
		return fmt.Errorf("the name must be a plain file name, got %q", name)
	}
	return nil
}

// validateSecrets rejects secret names that cannot name the credential
// file of section 8.1.
func validateSecrets(path string, m *Manifest) error {
	names := make([]string, 0, len(m.Secrets))
	for name := range m.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := nameReference(name); err != nil {
			return manifestErr(path, "secrets."+name, "%s", err)
		}
	}
	for id := range m.Operations {
		spec := m.Operations[id]
		if len(spec.Secrets) == 0 {
			continue
		}
		for _, name := range spec.Secrets {
			if _, ok := m.Secrets[name]; !ok {
				return manifestErr(path, opField(id, "secrets"),
					"secret %q is not declared in the capability's secrets", name)
			}
		}
	}
	return nil
}

// validateOperation validates one operation's non-schema rules
// (sections 6.2 and 10).
func validateOperation(path string, m *Manifest, opID string, spec *OperationSpec) error {
	if strings.TrimSpace(spec.Description) == "" {
		return manifestErr(path, opField(opID, "description"), "description is required")
	}
	switch spec.Mode {
	case "", ModeSync, ModeAsync:
	default:
		return manifestErr(path, opField(opID, "mode"), "mode must be %q or %q, got %q", ModeSync, ModeAsync, spec.Mode)
	}
	if _, err := parseOptionalDuration(spec.Timeout); err != nil {
		return manifestErr(path, opField(opID, "timeout"), "%s", err)
	}
	if _, err := parseOptionalDuration(spec.GracePeriod); err != nil {
		return manifestErr(path, opField(opID, "grace_period"), "%s", err)
	}
	switch m.Handler.Kind {
	case KindExec:
		for osname, cmd := range spec.Entry {
			if err := entryCommand(osname, cmd); err != nil {
				return manifestErr(path, opField(opID, "entry."+osname), "%s", err)
			}
		}
		if spec.URL != "" {
			return manifestErr(path, opField(opID, "url"), "url is http only")
		}
	case KindHTTP:
		if spec.Privileged {
			return manifestErr(path, opField(opID, "privileged"), "privileged is allowed only on exec operations")
		}
		for osname := range spec.Entry {
			return manifestErr(path, opField(opID, "entry."+osname), "entry is exec only")
		}
		if spec.URL != "" {
			if err := validateURL(spec.URL); err != nil {
				return manifestErr(path, opField(opID, "url"), "%s", err)
			}
		}
	}
	return nil
}

// parseOptionalDuration parses a manifest duration that may be empty.
// config.ParseDuration does the format check; the registry adds the
// manifest rule that a duration must not be negative.
func parseOptionalDuration(s string) (*time.Duration, error) {
	if s == "" {
		return nil, nil
	}
	d, err := config.ParseDuration(s)
	if err != nil {
		return nil, err
	}
	if d < 0 {
		return nil, fmt.Errorf("duration %q must not be negative", s)
	}
	return &d, nil
}

// validateURL checks a handler URL: http or https scheme with a host.
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %s", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("URL %q needs a host", raw)
	}
	return nil
}
