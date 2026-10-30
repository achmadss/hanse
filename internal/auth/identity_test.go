package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authorizeOK is the inner handler Middleware calls after it
// authenticates; it reports 200 when the context held an identity
// allowed to run demo.run. Used by middleware tests.
func authorizeOK(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r)
	if id == nil {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	if err := id.Authorize("demo", "run"); err != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// TestAuthenticateCodes runs the acceptance cases for the Authenticate
// return values: missing, wrong, allowed, forbidden, and revoked.
func TestAuthenticateCodes(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	full, err := s.Add("full", nil)
	if err != nil {
		t.Fatalf("Add full: %v", err)
	}
	limited, err := s.Add("limited", []string{"other.op"})
	if err != nil {
		t.Fatalf("Add limited: %v", err)
	}

	// Missing token: empty string authenticates to nothing.
	if _, err := s.Authenticate(""); err == nil {
		t.Errorf("missing token: want 401 AuthenticationFailed")
	} else if e := err; e.Error() != "authentication_failed: authentication failed" {
		t.Errorf("missing token error = %q, want the authentication_failed message", e.Error())
	}

	wrong := full[:len(full)-1] + "X"
	if _, err := s.Authenticate(wrong); err == nil {
		t.Errorf("wrong token: want 401")
	} else if strings.Contains(err.Error(), wrong) {
		t.Errorf("the error message holds the wrong token: %q", err.Error())
	}

	// Wrong-token errors are byte-identical to missing-token errors, so
	// the response never leaks which half of the credential failed.
	_, errMissing := s.Authenticate("")
	_, errWrong := s.Authenticate(wrong)
	if errMissing.Error() != errWrong.Error() {
		t.Errorf("missing and wrong errors differ: %q vs %q", errMissing.Error(), errWrong.Error())
	}

	id, err := s.Authenticate(full)
	if err != nil {
		t.Fatalf("Authenticate full: %v", err)
	}
	if err := id.Authorize("demo", "run"); err != nil {
		t.Errorf("Authorize with an empty allow list: %v", err)
	}

	idL, err := s.Authenticate(limited)
	if err != nil {
		t.Fatalf("Authenticate limited: %v", err)
	}
	if err := idL.Authorize("demo", "run"); err == nil {
		t.Errorf("Authorize demo.run with allow [other.op]: want Forbidden")
	} else if e := err; e.Error() != "forbidden: forbidden" {
		t.Errorf("forbidden error = %q, want the forbidden message", e.Error())
	}
	if err := idL.Authorize("other", "op"); err != nil {
		t.Errorf("Authorize other.op with allow [other.op]: %v", err)
	}
}

func TestMiddleware(t *testing.T) {
	p := storePath(t)
	s, _ := LoadStore(p)
	plain, err := s.Add("ci", []string{"demo.run"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h := s.Middleware(http.HandlerFunc(authorizeOK))
	t.Run("allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/operations", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("unauthorized handler", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/operations", nil))
		if rec.Code != 401 {
			t.Errorf("status = %d, want 401 before the handler runs", rec.Code)
		}
	})

	t.Run("missing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/operations", nil))
		if rec.Code != 401 {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		var body struct{ Code, Message string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
		}
		if body.Code != "authentication_failed" {
			t.Errorf("code = %q, want authentication_failed", body.Code)
		}
	})

	t.Run("wrong", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/operations", nil)
		req.Header.Set("Authorization", "Bearer not_a_real_token")
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		if rec.Body.String() != `{"code":"authentication_failed","message":"authentication failed"}` &&
			strings.ReplaceAll(rec.Body.String(), "\n", "") != `{"code":"authentication_failed","message":"authentication failed"}` {
			t.Errorf("body = %s, want the checkpoint error object", rec.Body.String())
		}
	})

	t.Run("allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/operations", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		limited, _ := s.Add("limited", []string{"other.op"})
		h2 := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := IdentityFrom(r).Authorize("demo", "run"); err != nil {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(struct{ Code string }{"forbidden"})
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/operations", nil)
		req.Header.Set("Authorization", "Bearer "+limited)
		h2.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("status = %d, want 403 (forbidden when the token lacks permission)", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"forbidden"`) {
			t.Errorf("body = %s, want the forbidden code", rec.Body.String())
		}
	})

	t.Run("revoked", func(t *testing.T) {
		mine, err := s.Add("to-revoke", nil)
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := s.Revoke("to-revoke"); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		after, err := LoadStore(p)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/operations", nil)
		req.Header.Set("Authorization", "Bearer "+mine)
		after.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := IdentityFrom(r).Authorize("demo", "run"); err != nil {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(struct{ Code string }{"forbidden"})
				return
			}
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("status = %d, want 401 for a revoked token", rec.Code)
		}
	})
}

func TestBearerToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := BearerToken(req); got != "" {
		t.Errorf("no header: %q, want empty", got)
	}
	for _, v := range []string{"token", "Bearer", "Basic dXNlcjpwYXNz"} {
		req.Header.Set("Authorization", v)
		if got := BearerToken(req); got != "" {
			t.Errorf("scheme %q: got %q, want empty", v, got)
		}
	}
	req.Header.Set("Authorization", "Bearer abc")
	if got := BearerToken(req); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
	// HTTP states are case-insensitive for the scheme.
	req.Header.Set("Authorization", "bearer abc")
	if got := BearerToken(req); got != "abc" {
		t.Errorf("lowercase scheme: got %q, want abc", got)
	}
}
