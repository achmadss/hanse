package api

import (
	"net/http"

	"github.com/achmadss/hanse/internal/auth"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// reloadWarningView is one non-fatal problem of a reload, such as a
// capability that routes.yaml does not mention.
type reloadWarningView struct {
	Capability string `json:"capability"`
	Message    string `json:"message"`
}

// reloadResponse is the body of POST /v1/capabilities/reload: the new
// capability set and the warnings of the reload.
type reloadResponse struct {
	Capabilities []capabilityView    `json:"capabilities"`
	Warnings     []reloadWarningView `json:"warnings"`
}

// reloadCapabilities answers POST /v1/capabilities/reload. It rescans the
// configured capability directories and swaps the result into the live
// registry, so an install or a removal reaches the Gateway without a
// restart.
//
// Only a token without an allow list, which has full access, may call it: a
// restricted token must not change what the Gateway runs. A restricted token
// answers the Forbidden code, HTTP 403.
func (s *server) reloadCapabilities(w http.ResponseWriter, r *http.Request) {
	id := auth.IdentityFrom(r)
	if id == nil {
		writeError(w, herrors.New(herrors.AuthenticationFailed,
			"the request has no authenticated identity", nil))
		return
	}
	if len(id.Allow()) != 0 {
		writeError(w, herrors.New(herrors.Forbidden,
			"reloading capabilities needs a token without an allow list", nil))
		return
	}
	if s.reg == nil {
		writeError(w, errNoRegistry())
		return
	}
	warnings, err := s.reg.Reload()
	if err != nil {
		writeError(w, herrors.New(herrors.Internal, "cannot reload capabilities: "+err.Error(), nil))
		return
	}
	caps := s.reg.Capabilities()
	out := reloadResponse{
		Capabilities: make([]capabilityView, 0, len(caps)),
		Warnings:     make([]reloadWarningView, 0, len(warnings)),
	}
	for _, c := range caps {
		out.Capabilities = append(out.Capabilities, capabilityViewOf(c))
	}
	for _, warning := range warnings {
		out.Warnings = append(out.Warnings, reloadWarningView{
			Capability: warning.Capability,
			Message:    warning.Message,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
