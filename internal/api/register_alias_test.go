package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRegisterSignupValidationAndSharedRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{signupPolicy: signupPolicy{disposableDomains: map[string]struct{}{"blocked.invalid": {}}}}
	r := gin.New()
	r.POST("/signup", s.signup)
	r.POST("/register", s.register)
	run := func(route, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	for _, body := range []string{`{}`, `{"email":"bad"}`, `{"email":"user@example.com","password":"password123"}`, `{"email":"user@blocked.invalid"}`} {
		a, b := run("/signup", body), run("/register", body)
		if a.Code != http.StatusBadRequest || b.Code != a.Code || b.Body.String() != a.Body.String() {
			t.Fatalf("validation differs: signup=%d %s register=%d %s", a.Code, a.Body.String(), b.Code, b.Body.String())
		}
	}
	s.signupLimiter = newSignupLimiter(1, time.Hour)
	// Rejected disposable signup still consumes an attempt; changing the public
	// endpoint cannot reset that IP's signup budget.
	if w := run("/signup", `{"email":"user@blocked.invalid"}`); w.Code != http.StatusBadRequest {
		t.Fatal(w.Code)
	}
	if w := run("/register", `{"email":"user@example.com"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("register bypassed shared limiter: %d", w.Code)
	}
}
