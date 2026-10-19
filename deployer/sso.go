package deployer

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Shared single-sign-on cookie across every *.osmosis.work admin surface. The
// exact wire format is mirrored, byte for byte, in remote-control-server's
// server.py and crawler-core's SsoCookie so a cookie minted by any of them
// validates in all of them:
//
//	value = base64url( "<user>:<exp>:<sig>" )
//	sig   = lowercase-hex HMAC-SHA256( key = SSO_SIGNING_KEY, msg = "<user>:<exp>" )
//
// The key is a shared random secret (SSO_SIGNING_KEY, from /root/.secrets/admin-auth.env),
// deliberately independent of the admin password since 2026-07-20 — so the bcrypt
// hash that lives in git history can never forge a cookie. Rotating the password
// does NOT invalidate outstanding cookies; rotating SSO_SIGNING_KEY does.
const (
	ssoCookieName = "osmosis_sso"
	ssoDomain     = ".osmosis.work"
	ssoMaxAge     = 90 * 24 * time.Hour
)

func ssoSign(key, payload string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

func ssoMint(key, user string, exp int64) string {
	payload := user + ":" + strconv.FormatInt(exp, 10)
	raw := payload + ":" + ssoSign(key, payload)
	return base64.URLEncoding.EncodeToString([]byte(raw))
}

// ssoValid reports whether cookie is a well-signed, unexpired token for `now`.
func ssoValid(key, cookie string, now int64) bool {
	if key == "" || cookie == "" {
		return false
	}
	rawb, err := base64.URLEncoding.DecodeString(cookie)
	if err != nil {
		return false
	}
	raw := string(rawb)
	// Split off the last two ":" fields, matching Python's rsplit(":", 2).
	i2 := strings.LastIndex(raw, ":")
	if i2 < 0 {
		return false
	}
	i1 := strings.LastIndex(raw[:i2], ":")
	if i1 < 0 {
		return false
	}
	payload, sig := raw[:i2], raw[i2+1:] // payload == "<user>:<exp>"
	if subtle.ConstantTimeCompare([]byte(sig), []byte(ssoSign(key, payload))) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(raw[i1+1:i2], 10, 64)
	if err != nil {
		return false
	}
	return exp > now
}

// clearSSO expires the shared cookie on the parent domain (global logout).
func clearSSO(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     ssoCookieName,
		Value:    "",
		Domain:   ssoDomain,
		Path:     "/",
		HttpOnly: true,
		Secure:   isTLS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// ssoAuthed reports whether the request carries a valid shared SSO cookie.
func ssoAuthed(r *http.Request) bool {
	c, err := r.Cookie(ssoCookieName)
	if err != nil {
		return false
	}
	return ssoValid(ssoKey, c.Value, time.Now().Unix())
}
