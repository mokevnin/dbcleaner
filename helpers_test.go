package pgcleaner_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// replaceDatabase points a server URL at another database on it.
func replaceDatabase(t testing.TB, serverURL, name string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	require.NoError(t, err)
	u.Path = "/" + name
	return u.String()
}

// databaseName is the database a URL points at.
func databaseName(t testing.TB, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return strings.TrimPrefix(u.Path, "/")
}
