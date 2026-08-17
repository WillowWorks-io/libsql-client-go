package libsql

import (
	"strings"
	"testing"
)

const sampleJWT = "eyJhbGciOiJFZERTQSIsInR5cCI6IkpXVCJ9.eyJhIjoicnciLCJpYXQiOjE3NzkwNjk3ODV9.QldxF6Lh7DQLKmouHqx8w8zVEYNZoPzZmV1PVvEjGH5uHDtwFc0D2iZnpLmO"

func TestRedactURLHidesTheToken(t *testing.T) {
	for _, param := range []string{"authToken", "auth_token", "jwt"} {
		got := RedactURL("libsql://db.turso.io?" + param + "=" + sampleJWT)
		if strings.Contains(got, sampleJWT) {
			t.Errorf("%s: token survived redaction: %s", param, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("%s: expected REDACTED marker, got %s", param, got)
		}
		if !strings.Contains(got, "db.turso.io") {
			t.Errorf("%s: host should survive so the message stays useful, got %s", param, got)
		}
	}
}

func TestRedactURLKeepsHarmlessDetail(t *testing.T) {
	got := RedactURL("libsql://db.turso.io?authToken=" + sampleJWT + "&tls=1")
	if !strings.Contains(got, "tls=1") {
		t.Errorf("non-credential parameters should survive: %s", got)
	}
}

func TestRedactURLHandlesNoCredential(t *testing.T) {
	in := "file:./local.db"
	if got := RedactURL(in); got != in {
		t.Errorf("a URL with nothing to hide should pass through: got %q want %q", got, in)
	}
	if got := RedactURL(""); got != "" {
		t.Errorf("empty in, empty out; got %q", got)
	}
}

func TestRedactURLHidesUserinfoPassword(t *testing.T) {
	got := RedactURL("libsql://user:hunter2@db.turso.io")
	if strings.Contains(got, "hunter2") {
		t.Errorf("password survived: %s", got)
	}
}

// A token under an unrecognised parameter name would slip past the allow-list,
// so the result is scanned for anything JWT-shaped before it is handed back.
func TestRedactURLWithholdsUnrecognisedCredential(t *testing.T) {
	got := RedactURL("libsql://db.turso.io?token=" + sampleJWT)
	if strings.Contains(got, sampleJWT) {
		t.Errorf("JWT-shaped value under an unknown key was returned: %s", got)
	}
}

func TestRedactURLDoesNotReturnUnparseableInput(t *testing.T) {
	got := RedactURL("://not a url?authToken=" + sampleJWT)
	if strings.Contains(got, sampleJWT) {
		t.Errorf("unparseable input echoed a credential: %s", got)
	}
}
