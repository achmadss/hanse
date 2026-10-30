package auth

import (
	"context"
	"net/http"
	"strings"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// Identity is the authenticated token of one request: its name for the
// audit log and its allow list.
type Identity struct {
	Name  string
	allow []string
}

// Allow returns a copy of this identity's allow list.
func (id *Identity) Allow() []string {
	if id == nil {
		return nil
	}
	return append([]string(nil), id.allow...)
}

// Authorize decides whether the identity may run cap.op. An allow list
// matches "<capability>.<operation>" entries exactly, and "<capability>"
// entries for every operation of that capability (architecture.md §15).
// An empty allow list allows everything. The error is the Forbidden
// code, HTTP 403 (architecture.md §14); it never names the token value.
func (id *Identity) Authorize(capID, opID string) error {
	if id == nil {
		return herrors.New(herrors.Forbidden, "", nil)
	}
	if len(id.allow) == 0 {
		return nil
	}
	for _, entry := range id.allow {
		if entry == capID || entry == capID+"."+opID {
			return nil
		}
	}
	return herrors.New(herrors.Forbidden, "", nil)
}

// Authenticate checks a presented plain token. A missing, wrong, or
// revoked token is the AuthenticationFailed code, HTTP 401
// (architecture.md §14); the error and the store never contain a token
// value. Before checking, Authenticate stats the tokens file and
// reloads it when it changed, so `hanse-gateway token revoke` reaches a
// running Gateway. When the file cannot be reloaded (unreadable,
// invalid, or unsafe), authentication fails closed: every request
// answers 401 until the file is fixed. The refusal of a failed reload
// is not sticky, so a fixed file serves again.
func (s *Store) Authenticate(plain string) (*Identity, error) {
	if plain == "" {
		return nil, herrors.New(herrors.AuthenticationFailed, "", nil)
	}
	hash := Hash(plain)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changedLocked() {
		if err := s.loadLocked(); err != nil {
			return nil, herrors.New(herrors.AuthenticationFailed, "", nil)
		}
	}
	for _, r := range s.tokens {
		if hashesMatch(r.Hash, hash) {
			return &Identity{Name: r.Name, allow: append([]string(nil), r.Allow...)}, nil
		}
	}
	return nil, herrors.New(herrors.AuthenticationFailed, "", nil)
}

// ctxKey-type keeps context values unexported so other packages cannot
// forge them.
type ctxKey struct{}

// Middleware authenticates every request with its bearer token
// (architecture.md §4 step 1) and, when it succeeds, stores the
// Identity in the request context. It writes the shared error object
// for 401 with the code authentication_failed. The API package wires
// the resulting authorization (#16) through IdentityFrom and
// Identity.Authorize.
func (s *Store) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plain := BearerToken(r)
		id, err := s.Authenticate(plain)
		if err != nil {
			// RFC 6750: the challenge accompanies the 401.
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(herrors.HTTPStatus(err))
			_ = herrors.WriteJSON(w, err)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// IdentityFrom returns the authenticated Identity of the request, or
// nil before Middleware ran or after a failed authentication.
func IdentityFrom(r *http.Request) *Identity {
	id, _ := r.Context().Value(ctxKey{}).(*Identity)
	return id
}

// BearerToken extracts the bearer token of the request. It returns ""
// when the Authorization header is missing or does not carry the
// Bearer scheme. The token never reaches a log or an error message:
// callers keep it in the returned string only.
func BearerToken(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	const scheme = "Bearer "
	if len(authz) < len(scheme) || !strings.EqualFold(authz[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(authz[len(scheme):])
}
