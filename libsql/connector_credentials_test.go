package libsql

import (
	"strings"
	"testing"
)

// NewConnector used to reject any URL carrying authToken/auth_token/jwt/tls,
// telling the caller to use the matching option -- while Driver.Open, the other
// entry point to this same driver, parsed exactly those parameters out of
// exactly that URL.
//
// Turso hands out connection strings with ?authToken= in them, so the rejected
// form is the one practically every caller has. Anyone migrating from
// sql.Open to NewConnector got a startup failure the moment they deployed,
// which is how this was found: a production service exited at boot with
// "'authToken' usage forbidden".
//
// The tests below are deliberately about the REAL DSN shape rather than a
// synthetic one. Every test that existed for this path used either a fake
// connector or a file: database, and so proved nothing about the line that
// actually broke.

const turso = "libsql://db-org.aws-us-east-1.turso.io"

func TestNewConnectorAcceptsTokenInURL(t *testing.T) {
	for _, param := range []string{"authToken", "auth_token", "jwt"} {
		t.Run(param, func(t *testing.T) {
			if _, err := NewConnector(turso + "?" + param + "=sometoken"); err != nil {
				t.Fatalf("a URL carrying %s must be accepted, as Driver.Open accepts it: %v", param, err)
			}
		})
	}
}

func TestNewConnectorAcceptsTokenAsOption(t *testing.T) {
	if _, err := NewConnector(turso, WithAuthToken("sometoken")); err != nil {
		t.Fatalf("WithAuthToken: %v", err)
	}
}

// The option and the URL may both be present as long as they agree -- a caller
// normalising a DSN while also passing the token should not be punished for it.
func TestNewConnectorAcceptsAgreeingToken(t *testing.T) {
	if _, err := NewConnector(turso+"?authToken=same", WithAuthToken("same")); err != nil {
		t.Fatalf("identical token from both sources should be fine: %v", err)
	}
}

// Disagreement is reported rather than silently resolved: whichever one we
// picked, the caller has a bug worth hearing about.
func TestNewConnectorRejectsConflictingTokens(t *testing.T) {
	_, err := NewConnector(turso+"?authToken=from-url", WithAuthToken("from-option"))
	if err == nil {
		t.Fatal("two different tokens must be an error, not a silent pick")
	}
	if !strings.Contains(err.Error(), "conflicting auth tokens") {
		t.Errorf("error should name the conflict, got: %v", err)
	}
}

func TestNewConnectorAcceptsTlsInURL(t *testing.T) {
	if _, err := NewConnector(turso + "?authToken=t&tls=1"); err != nil {
		t.Fatalf("tls=1 in the URL must be accepted: %v", err)
	}
	// tls=0 on libsql:// downgrades to http, which the driver requires an
	// explicit port for -- the same rule Driver.Open enforces.
	if _, err := NewConnector("libsql://db-org.turso.io:8080?tls=0"); err != nil {
		t.Fatalf("tls=0 with an explicit port must be accepted: %v", err)
	}
}

func TestNewConnectorRejectsConflictingTls(t *testing.T) {
	_, err := NewConnector(turso+"?tls=1", WithTls(false))
	if err == nil {
		t.Fatal("URL and WithTls disagreeing must be an error")
	}
	if !strings.Contains(err.Error(), "conflicting tls") {
		t.Errorf("error should name the conflict, got: %v", err)
	}
}

// A scheme-derived TLS default must not be mistaken for the caller stating one,
// or WithTls would appear to conflict with a URL that said nothing about TLS.
func TestSchemeDefaultDoesNotConflictWithWithTls(t *testing.T) {
	if _, err := NewConnector("libsql://db-org.turso.io:8080?authToken=t", WithTls(false)); err != nil {
		t.Fatalf("WithTls with no tls= in the URL must not report a conflict: %v", err)
	}
}

// Unknown parameters are still rejected -- accepting credentials is not licence
// to accept typos.
func TestNewConnectorStillRejectsUnknownParameters(t *testing.T) {
	if _, err := NewConnector(turso + "?authTokn=typo"); err == nil {
		t.Fatal("an unknown query parameter should still be an error")
	}
}
