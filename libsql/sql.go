package libsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/WillowWorks-io/libsql-client-go/libsql/internal/http"
	"github.com/WillowWorks-io/libsql-client-go/libsql/internal/ws"
)

type config struct {
	authToken           *string
	tls                 *bool
	proxy               *string
	schemaDb            *bool
	remoteEncryptionKey *string
	requestHeaders      map[string]string
	keepaliveInterval   *time.Duration
}

type Option interface {
	apply(*config) error
}

type option func(*config) error

func (o option) apply(c *config) error {
	return o(c)
}

func WithAuthToken(authToken string) Option {
	return option(func(o *config) error {
		if o.authToken != nil {
			return fmt.Errorf("authToken already set")
		}
		if authToken == "" {
			return fmt.Errorf("authToken must not be empty")
		}
		o.authToken = &authToken
		return nil
	})
}

func WithTls(tls bool) Option {
	return option(func(o *config) error {
		if o.tls != nil {
			return fmt.Errorf("tls already set")
		}
		o.tls = &tls
		return nil
	})
}

func WithProxy(proxy string) Option {
	return option(func(o *config) error {
		if o.proxy != nil {
			return fmt.Errorf("proxy already set")
		}
		if proxy == "" {
			return fmt.Errorf("proxy must not be empty")
		}
		o.proxy = &proxy
		return nil
	})
}

func WithSchemaDb(schemaDb bool) Option {
	return option(func(o *config) error {
		if o.schemaDb != nil {
			return fmt.Errorf("schemaDb already set")
		}
		o.schemaDb = &schemaDb
		return nil
	})
}

func WithRemoteEncryptionKey(key string) Option {
	return option(func(o *config) error {
		if o.remoteEncryptionKey != nil {
			return fmt.Errorf("remoteEncryptionKey already set")
		}
		if key == "" {
			return fmt.Errorf("remoteEncryptionKey must not be empty")
		}
		o.remoteEncryptionKey = &key
		return nil
	})
}

// WithRequestHeaders attaches arbitrary HTTP headers to every request the
// driver sends to the remote server. Passing the `Host` key (case-insensitive)
// has no effect.
func WithRequestHeaders(headers map[string]string) Option {
	return option(func(o *config) error {
		if o.requestHeaders != nil {
			return fmt.Errorf("requestHeaders already set")
		}
		if len(headers) == 0 {
			return fmt.Errorf("requestHeaders must not be empty")
		}
		copied := make(map[string]string, len(headers))
		for k, v := range headers {
			copied[k] = v
		}
		o.requestHeaders = copied
		return nil
	})
}

func (c config) connector(dbPath string) (driver.Connector, error) {
	u, err := url.Parse(dbPath)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "file" {
		if strings.HasPrefix(dbPath, "file://") && !strings.HasPrefix(dbPath, "file:///") {
			return nil, fmt.Errorf("invalid database URL: %s. File URLs should not have double leading slashes. ", dbPath)
		}
		expectedDrivers := []string{"sqlite", "sqlite3"}
		presentDrivers := sql.Drivers()
		for _, expectedDriver := range expectedDrivers {
			if Contains(presentDrivers, expectedDriver) {
				db, err := sql.Open(expectedDriver, dbPath)
				if err != nil {
					return nil, err
				}
				return &fileConnector{url: dbPath, driver: db.Driver()}, nil
			}
		}
		return nil, fmt.Errorf("no sqlite driver present. Please import sqlite or sqlite3 driver")
	}

	// Credentials may arrive either in the URL or as options.
	//
	// This used to reject a URL carrying authToken/auth_token/jwt/tls outright,
	// telling the caller to use WithAuthToken instead -- while Driver.Open, the
	// other entry point to this same driver, happily accepted exactly that URL
	// via extractJwt. Turso hands out connection strings with ?authToken= in
	// them, so NewConnector rejected the format practically every caller has,
	// and anyone migrating from sql.Open to NewConnector hit it the moment they
	// deployed. Refusing to parse a URL the sibling entry point parses is a bug,
	// not a policy.
	//
	// So: parse them. An explicit option still wins, since it is the more
	// specific instruction, and disagreeing sources are reported rather than
	// silently resolved -- a caller passing two different tokens has a bug worth
	// hearing about.
	query := u.Query()

	urlToken, err := extractJwt(&query)
	if err != nil {
		return nil, err
	}
	if urlToken != "" {
		switch {
		case c.authToken == nil:
			c.authToken = &urlToken
		case *c.authToken != urlToken:
			return nil, fmt.Errorf("conflicting auth tokens: the URL and WithAuthToken disagree; pass one or the other")
		}
	}

	// Note extractTls also supplies the scheme-derived default when the URL has
	// no tls parameter, so "was it stated?" has to be captured before the call
	// deletes it -- otherwise a scheme default would look like an explicit
	// setting and could contradict WithTls for no reason.
	tlsStated := query.Has("tls")
	urlTls, err := extractTls(&query, u.Scheme)
	if err != nil {
		return nil, err
	}
	switch {
	case c.tls == nil:
		c.tls = &urlTls
	case tlsStated && *c.tls != urlTls:
		return nil, fmt.Errorf("conflicting tls settings: the URL and WithTls disagree; pass one or the other")
	}

	for name := range query {
		return nil, fmt.Errorf("unknown query parameter %#v", name)
	}

	if u.Scheme == "libsql" {
		if c.tls == nil || *c.tls {
			u.Scheme = "https"
		} else {
			if c.tls != nil && u.Port() == "" {
				return nil, fmt.Errorf("libsql:// URL without tls must specify an explicit port")
			}
			u.Scheme = "http"
		}
	}

	if (u.Scheme == "wss" || u.Scheme == "https") && c.tls != nil && !*c.tls {
		return nil, fmt.Errorf("%s:// URL cannot opt out of TLS. Only libsql:// can opt in/out of TLS", u.Scheme)
	}
	if (u.Scheme == "ws" || u.Scheme == "http") && c.tls != nil && *c.tls {
		return nil, fmt.Errorf("%s:// URL cannot opt in to TLS. Only libsql:// can opt in/out of TLS", u.Scheme)
	}

	authToken := ""
	if c.authToken != nil {
		authToken = *c.authToken
	}
	encryptionKey := ""
	if c.remoteEncryptionKey != nil {
		encryptionKey = *c.remoteEncryptionKey
	}

	host := u.Host
	if c.proxy != nil {
		if u.Scheme == "ws" || u.Scheme == "wss" {
			return nil, fmt.Errorf("proxying of ws:// and wss:// URLs is not supported")
		}
		proxy, err := url.Parse(*c.proxy)
		if err != nil {
			return nil, err
		}
		u.Host = proxy.Host
		if proxy.Scheme != "" {
			u.Scheme = proxy.Scheme
		}
	}

	schemaDb := false
	if c.schemaDb != nil {
		schemaDb = *c.schemaDb
	}

	if u.Scheme == "wss" || u.Scheme == "ws" {
		return wsConnector{url: u.String(), authToken: authToken}, nil
	}
	if u.Scheme == "https" || u.Scheme == "http" {
		return httpConnector{url: u.String(), authToken: authToken, host: host, schemaDb: schemaDb, remoteEncryptionKey: encryptionKey, requestHeaders: c.requestHeaders}, nil
	}

	return nil, fmt.Errorf("unsupported URL scheme: %s\nThis driver supports only URLs that start with libsql://, file://, https://, http://, wss:// and ws://", u.Scheme)
}

func NewConnector(dbPath string, opts ...Option) (driver.Connector, error) {
	var config config
	errs := make([]error, 0, len(opts))
	for _, opt := range opts {
		if err := opt.apply(&config); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	c, err := config.connector(dbPath)
	if err != nil {
		return nil, err
	}
	return config.withKeepAlive(c), nil
}

// withKeepAlive decorates a remote connector when WithKeepAlive asked for it.
//
// Deliberately silent in the cases where it does not apply, rather than an
// error: a caller wiring the interval to configuration should be able to pass
// the same options for a local file: database and a remote one without
// branching, and a keepalive against local disk is meaningless rather than
// wrong. A non-positive interval disables it for the same reason.
func (c config) withKeepAlive(inner driver.Connector) driver.Connector {
	if c.keepaliveInterval == nil || *c.keepaliveInterval <= 0 {
		return inner
	}
	if _, isFile := inner.(*fileConnector); isFile {
		return inner
	}
	return newKeepaliveConnector(inner, *c.keepaliveInterval)
}

type httpConnector struct {
	url                 string
	authToken           string
	host                string
	schemaDb            bool
	remoteEncryptionKey string
	requestHeaders      map[string]string
}

func (c httpConnector) Connect(_ctx context.Context) (driver.Conn, error) {
	return http.Connect(c.url, c.authToken, c.host, c.schemaDb, c.remoteEncryptionKey, c.requestHeaders), nil
}

func (c httpConnector) Driver() driver.Driver {
	return Driver{}
}

type wsConnector struct {
	url       string
	authToken string
}

func (c wsConnector) Connect(_ctx context.Context) (driver.Conn, error) {
	return ws.Connect(c.url, c.authToken)
}

func (c wsConnector) Driver() driver.Driver {
	return Driver{}
}

type fileConnector struct {
	url    string
	driver driver.Driver
}

func (c fileConnector) Connect(_ctx context.Context) (driver.Conn, error) {
	return c.driver.Open(c.url)
}

func (c fileConnector) Driver() driver.Driver {
	return Driver{}
}

type Driver struct{}

// ExtractJwt extracts the JWT from the URL and removes it from the url.
func extractJwt(query *url.Values) (string, error) {
	authTokenSnake := query.Get("auth_token")
	authTokenCamel := query.Get("authToken")
	jwt := query.Get("jwt")
	query.Del("auth_token")
	query.Del("authToken")
	query.Del("jwt")

	countNonEmpty := func(slice ...string) int {
		count := 0
		for _, s := range slice {
			if s != "" {
				count++
			}
		}
		return count
	}

	if countNonEmpty(authTokenSnake, authTokenCamel, jwt) > 1 {
		return "", fmt.Errorf("please use at most one of the following query parameters: 'auth_token', 'authToken', 'jwt'")
	}

	if authTokenSnake != "" {
		return authTokenSnake, nil
	} else if authTokenCamel != "" {
		return authTokenCamel, nil
	} else {
		return jwt, nil
	}
}

func extractTls(query *url.Values, scheme string) (bool, error) {
	tls := query.Get("tls")
	query.Del("tls")
	switch tls {
	case "":
		if scheme == "http" || scheme == "ws" {
			return false, nil
		} else {
			return true, nil
		}
	case "0":
		return false, nil

	case "1":

		return true, nil
	default:
		return true, fmt.Errorf("unknown value of tls query parameter. Valid values are 0 and 1")
	}
}

func (d Driver) Open(dbUrl string) (driver.Conn, error) {
	u, err := url.Parse(dbUrl)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "file" {
		if strings.HasPrefix(dbUrl, "file://") && !strings.HasPrefix(dbUrl, "file:///") {
			return nil, fmt.Errorf("invalid database URL: %s. File URLs should not have double leading slashes. ", dbUrl)
		}
		expectedDrivers := []string{"sqlite", "sqlite3"}
		presentDrivers := sql.Drivers()
		for _, expectedDriver := range expectedDrivers {
			if Contains(presentDrivers, expectedDriver) {
				db, err := sql.Open(expectedDriver, dbUrl)
				if err != nil {
					return nil, err
				}
				return db.Driver().Open(dbUrl)
			}
		}
		return nil, fmt.Errorf("no sqlite driver present. Please import sqlite or sqlite3 driver")
	}

	query := u.Query()
	jwt, err := extractJwt(&query)
	if err != nil {
		return nil, err
	}

	tls, err := extractTls(&query, u.Scheme)
	if err != nil {
		return nil, err
	}

	for name := range query {
		return nil, fmt.Errorf("unknown query parameter %#v", name)
	}
	u.RawQuery = ""

	if u.Scheme == "libsql" {
		if tls {
			u.Scheme = "https"
		} else {
			if u.Port() == "" {
				return nil, fmt.Errorf("libsql:// URL with ?tls=0 must specify an explicit port")
			}
			u.Scheme = "http"
		}
	}

	if (u.Scheme == "wss" || u.Scheme == "https") && !tls {
		return nil, fmt.Errorf("%s:// URL cannot opt out of TLS using ?tls=0", u.Scheme)
	}
	if (u.Scheme == "ws" || u.Scheme == "http") && tls {
		return nil, fmt.Errorf("%s:// URL cannot opt in to TLS using ?tls=1", u.Scheme)
	}

	if u.Scheme == "wss" || u.Scheme == "ws" {
		return ws.Connect(u.String(), jwt)
	}
	if u.Scheme == "https" || u.Scheme == "http" {
		return http.Connect(u.String(), jwt, u.Host, false, "", nil), nil
	}

	return nil, fmt.Errorf("unsupported URL scheme: %s\nThis driver supports only URLs that start with libsql://, file://, https://, http://, wss:// and ws://", u.Scheme)
}

func init() {
	sql.Register("libsql", Driver{})
}

// backported from Go 1.21

func Contains[S ~[]E, E comparable](s S, v E) bool {
	return Index(s, v) >= 0
}

func Index[S ~[]E, E comparable](s S, v E) int {
	for i := range s {
		if v == s[i] {
			return i
		}
	}
	return -1
}
