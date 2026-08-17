package libsql

import (
	"net/url"
	"strings"
)

// RedactURL returns a connection URL safe to put in a log line or an error
// message, with any credential replaced by "REDACTED".
//
// This exists because the natural thing to write is the dangerous thing:
//
//	return fmt.Errorf("opening database %q: %w", databaseURL, err)
//
// Turso issues connection strings with the auth token embedded as a query
// parameter, so that line writes a live credential into whatever collects the
// error -- a log aggregator, an error tracker, a CI transcript, a terminal
// someone screenshots. It is easy to miss precisely because the URL looks like
// configuration rather than a secret.
//
// Redacted parameters are authToken, auth_token and jwt (the three spellings
// this driver accepts) plus userinfo in the URL itself. Everything else --
// scheme, host, path, other parameters -- is preserved, since the point is to
// keep the message useful for diagnosis.
//
// A string that will not parse as a URL is not returned verbatim on the chance
// that it contains something sensitive; callers get a fixed placeholder.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable database URL)"
	}

	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}

	if q := u.Query(); len(q) > 0 {
		changed := false
		for _, key := range []string{"authToken", "auth_token", "jwt"} {
			if q.Has(key) {
				q.Set(key, "REDACTED")
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}

	out := u.String()
	// Belt and braces: if anything that looks like a JWT survived -- an
	// unrecognised parameter name, say -- do not hand it back.
	if looksLikeJWT(out) {
		return "(database URL withheld: possible credential)"
	}
	return out
}

// looksLikeJWT reports whether s contains a three-segment dotted token of the
// shape libSQL auth tokens use. Deliberately crude: a false positive costs a
// less specific error message, a false negative leaks a credential.
func looksLikeJWT(s string) bool {
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '=' || r == '&' || r == '/' || r == '?' || r == '@'
	}) {
		parts := strings.Split(field, ".")
		if len(parts) != 3 {
			continue
		}
		if len(parts[0]) >= 8 && len(parts[1]) >= 8 && len(parts[2]) >= 8 {
			return true
		}
	}
	return false
}
