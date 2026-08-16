package hranaV2

import (
	"database/sql/driver"
	"testing"
)

// These two interfaces are what database/sql uses to keep its pool honest, and
// this driver advertised neither before the fork. The consequences are not
// subtle -- see the doc comments on IsValid and Ping -- so they are pinned here
// rather than left to a compile-time assertion someone could delete while
// chasing a build error.

func TestConnAdvertisesValidatorAndPinger(t *testing.T) {
	var c any = &hranaV2Conn{}

	if _, ok := c.(driver.Validator); !ok {
		t.Error("hranaV2Conn must implement driver.Validator, or database/sql " +
			"pools connections whose stream the server has already closed")
	}
	if _, ok := c.(driver.Pinger); !ok {
		t.Error("hranaV2Conn must implement driver.Pinger, or DB.Ping() returns " +
			"without ever reaching the server")
	}
}

// TestIsValidTracksStreamState: the server closes the Hrana stream after each
// statement unless a transaction holds it open, and the connection is unusable
// from that point. Reporting it is what lets putConn discard the connection
// instead of handing it to the next caller.
func TestIsValidTracksStreamState(t *testing.T) {
	fresh := &hranaV2Conn{}
	if !fresh.IsValid() {
		t.Error("a connection whose stream is still open must report valid")
	}

	spent := &hranaV2Conn{streamClosed: true}
	if spent.IsValid() {
		t.Error("a connection whose stream the server closed must report invalid; " +
			"reusing it fails with \"stream is closed: driver: bad connection\"")
	}
}
