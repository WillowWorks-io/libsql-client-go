package libsql

import (
	"bytes"
	"log/slog"
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
			c, err := NewConnector(turso + "?" + param + "=sometoken")
			if err != nil {
				t.Fatalf("a URL carrying %s must be accepted, as Driver.Open accepts it: %v", param, err)
			}
			hc, ok := c.(httpConnector)
			if !ok {
				t.Fatalf("expected an httpConnector, got %T", c)
			}
			if hc.authToken != "sometoken" {
				t.Errorf("token from %s not picked up: authToken = %q", param, hc.authToken)
			}
			// Accepting the parameter is only half of it: the URL is used to
			// build the request path, so a token left in the query string turns
			// every request into "v2/pipeline?authToken=..." and a 404 -- with
			// the credential in the error. Constructing without error proves
			// nothing here; the URL has to be clean.
			if strings.Contains(hc.url, "sometoken") || strings.Contains(hc.url, "?") {
				t.Errorf("credential left in the connector URL: %q", hc.url)
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

// When the two disagree the OPTION wins. Connection strings often arrive from a
// platform as an injected secret, so requiring the URL to be edited before an
// option may be used would make the option useless exactly where it is needed.
//
// Asserting the resulting value, not merely the absence of an error: "no error"
// would pass just as well if the URL's token had silently won.
func TestOptionBeatsURLToken(t *testing.T) {
	c, err := NewConnector(turso+"?authToken=from-url", WithAuthToken("from-option"))
	if err != nil {
		t.Fatalf("a disagreement is a warning, not an error: %v", err)
	}
	hc, ok := c.(httpConnector)
	if !ok {
		t.Fatalf("expected an httpConnector, got %T", c)
	}
	if hc.authToken != "from-option" {
		t.Errorf("authToken = %q, want the WithAuthToken value to win", hc.authToken)
	}
}

// The warning must never carry the credentials themselves, or a config smell
// becomes a secret sitting in the log store.
func TestConflictWarningOmitsTheTokens(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	if _, err := NewConnector(turso+"?authToken=url-secret", WithAuthToken("option-secret")); err != nil {
		t.Fatalf("NewConnector: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "WithAuthToken") {
		t.Errorf("a conflict should be warned about; got: %q", logged)
	}
	for _, secret := range []string{"url-secret", "option-secret"} {
		if strings.Contains(logged, secret) {
			t.Errorf("the warning leaked a credential (%q): %s", secret, logged)
		}
	}
}

// Agreement is not a conflict and must stay quiet.
func TestAgreeingTokenLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	if _, err := NewConnector(turso+"?authToken=same", WithAuthToken("same")); err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("identical values are not a conflict; logged: %s", buf.String())
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

func TestOptionBeatsURLTls(t *testing.T) {
	// tls=0 downgrades libsql:// to http, which requires an explicit port; the
	// option overriding back to TLS is what keeps this on https.
	c, err := NewConnector("libsql://db-org.turso.io:8080?tls=0", WithTls(true))
	if err != nil {
		t.Fatalf("a disagreement is a warning, not an error: %v", err)
	}
	hc, ok := c.(httpConnector)
	if !ok {
		t.Fatalf("expected an httpConnector, got %T", c)
	}
	if !strings.HasPrefix(hc.url, "https://") {
		t.Errorf("url = %q, want WithTls(true) to win over tls=0", hc.url)
	}
}

// A scheme-derived TLS default must not be mistaken for the caller stating one,
// or WithTls would appear to conflict with a URL that said nothing about TLS.
func TestSchemeDefaultDoesNotConflictWithWithTls(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	if _, err := NewConnector("libsql://db-org.turso.io:8080?authToken=t", WithTls(false)); err != nil {
		t.Fatalf("WithTls with no tls= in the URL must not be a conflict: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a scheme-derived default is not the caller stating anything; logged: %s", buf.String())
	}
}

// Unknown parameters are still rejected -- accepting credentials is not licence
// to accept typos.
func TestNewConnectorStillRejectsUnknownParameters(t *testing.T) {
	if _, err := NewConnector(turso + "?authTokn=typo"); err == nil {
		t.Fatal("an unknown query parameter should still be an error")
	}
}
